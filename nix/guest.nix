{ config, lib, pkgs, modulesPath, ... }:
let
  agent = pkgs.buildGoModule {
    pname = "containers-anywhere-agent";
    version = lib.removeSuffix "\n" (builtins.readFile ../APPLIANCE_VERSION);
    src = lib.fileset.toSource {
      root = ../.;
      fileset = lib.fileset.unions [
        ../cmd/guest
        ../internal/guest
        ../internal/boottls
        ../internal/protocol
        ../internal/atomicfile
        ../go.mod
        ../go.sum
      ];
    };
    subPackages = [ "cmd/guest" ];
    vendorHash = "sha256-tJ4oFvPra0/k7Pn5fmvNCBFWA3v6PXOwX2xNWUVpziQ=";
    env.CGO_ENABLED = "0";
    ldflags = [ "-s" "-w" ];
    doCheck = false;
  };
  kite = import ./kite.nix { inherit pkgs; };
  gateway = import ./traefik.nix { inherit pkgs; };
  demo = import ./demo.nix { inherit pkgs; };
in
{
  imports = [ "${modulesPath}/profiles/minimal.nix" "${modulesPath}/profiles/headless.nix" ];
  system.stateVersion = "25.05";
  networking.hostName = "containers-anywhere";
  nix.enable = false;
  system.switch.enable = false;
  documentation.enable = false;
  environment.defaultPackages = [ ];
  # Keep basic shell/storage diagnostics; omit the interactive distro toolbox.
  # Services retain their own explicitly declared executable dependencies.
  environment.corePackages = lib.mkForce (with pkgs; [
    bash
    coreutils
    findutils
    gawk
    gnugrep
    gnused
    procps
    util-linux
  ]);
  # No interactive administration or physical-disk management in this appliance.
  programs.nano.enable = false;
  programs.less.enable = lib.mkForce false;
  environment.variables.PAGER = "cat";
  fonts.fontconfig.enable = false;
  services.lvm.enable = false;
  boot.bcache.enable = false;
  boot.kexec.enable = false;
  security.sudo.enable = false;
  microvm = {
    hypervisor = "qemu";
    mem = 4096;
    vcpu = 2;
    storeOnDisk = true;
    storeDiskType = "erofs";
    # -Ededupe forces single-threaded compression in the pinned microvm builder.
    # Retain LZ4HC density and tail packing while allowing parallel construction.
    storeDiskErofsFlags = [ "-zlz4hc" "-Eztailpacking" "-Efragments" ];
    volumes = [{ image = "data.img"; mountPoint = "/var/lib"; size = 16384; }];
  };
  fileSystems."/var/lib" = {
    device = lib.mkForce "/dev/disk/by-id/virtio-ca-data";
    autoResize = true;
    neededForBoot = true;
  };
  boot.initrd.supportedFilesystems = [ "erofs" "ext4" ];
  boot.kernelModules = [ "vmw_vsock_virtio_transport" "virtio_net" "qemu_fw_cfg" "virtio_balloon" "overlay" "br_netfilter" "nf_conntrack" ];
  boot.kernel.sysctl."net.ipv4.ip_forward" = 1;
  networking.useDHCP = true;
  # Stable source identity for the node's authenticated workload relay. It is
  # not a LAN/VM listener and is never forwarded by QEMU or Virtualization.framework.
  networking.interfaces.lo.ipv4.addresses = [{ address = "169.254.254.1"; prefixLength = 32; }];
  networking.firewall = {
    enable = true;
    allowedTCPPorts = [ 8080 ]; # Per-boot mTLS guest agent, never raw Kubernetes.
    trustedInterfaces = [ "cni0" "flannel.1" ];
    checkReversePath = "loose";
  };
  services.k3s = {
    enable = true;
    role = "server";
    nodeName = "containers-anywhere";
    disable = [ "traefik" "servicelb" "metrics-server" ];
    extraFlags = [ "--write-kubeconfig-mode=0600" "--secrets-encryption" ];
    gracefulNodeShutdown.enable = true;
    images = [ config.services.k3s.package.airgap-images kite gateway.image demo ];
    manifests.kite.source = ./kite.yaml;
    manifests.cncm.source = ./cncm.yaml;
    manifests.gateway.source = gateway.manifest;
    manifests.demo.source = pkgs.writeText "demo.yaml" (builtins.replaceStrings
      [ "@CADDY_VERSION@" ] [ pkgs.caddy.version ] (builtins.readFile ./demo.yaml));
  };
  # The root filesystem is disposable. Retain the node password across boots,
  # alongside the cluster database, containerd images and local-path volumes.
  systemd.tmpfiles.rules = [
    "d /var/lib/rancher/node 0700 root root -"
    "L+ /etc/rancher/node - - - - /var/lib/rancher/node"
    "d /var/lib/containers-anywhere/kite 0700 root root -"
  ];
  systemd.services.kite-secrets = {
    before = [ "k3s.service" ];
    requiredBy = [ "k3s.service" ];
    after = [ "systemd-tmpfiles-setup.service" ];
    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
      UMask = "0077";
      ExecStart = "${agent}/bin/guest guest init-kite";
    };
  };
  # A reconnecting local tunnel avoids NodePort/hostNetwork exposure and survives
  # Kite pod replacement. Both HTTP and WebSocket upgrades use the guest agent.
  systemd.services.kite-forward = {
    wantedBy = [ "multi-user.target" ];
    after = [ "k3s.service" ];
    # Requires cancels our start job if K3s initially fails (e.g. DHCP has no
    # default route yet). Restart= cannot recover a job that never executed.
    # Start independently and retry until the API and Kite pod are available.
    wants = [ "k3s.service" ];
    unitConfig.StartLimitIntervalSec = 0;
    serviceConfig = {
      ExecStart = "${config.services.k3s.package}/bin/k3s kubectl --kubeconfig=/etc/rancher/k3s/k3s.yaml -n kite-system port-forward --address=127.0.0.1 service/kite 8081:8080";
      Restart = "always";
      RestartSec = 5;
    };
  };
  systemd.services.containers-agent = {
    wantedBy = [ "multi-user.target" ];
    after = [ "local-fs.target" ];
    path = [ pkgs.systemd config.services.k3s.package ];
    serviceConfig = { ExecStart = "${agent}/bin/guest guest"; Restart = "on-failure"; };
  };
  services.fstrim.enable = true;
}
