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
        ../internal/publication
        ../internal/hostapi
        ../internal/storage
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
  consoleForward = pkgs.writeShellScript "console-forward" ''
    set -eu
    kubectl() { ${config.services.k3s.package}/bin/k3s kubectl --kubeconfig=/etc/rancher/k3s/k3s.yaml "$@"; }
    target=$(kubectl get service -A -l containers-anywhere.dev/console=true \
      -o 'jsonpath={.items[0].metadata.namespace} {.items[0].metadata.name} {.items[0].spec.ports[0].port}')
    set -- $target
    [ $# -eq 3 ] || { echo "no console service is published yet" >&2; exit 1; }
    exec ${config.services.k3s.package}/bin/k3s kubectl --kubeconfig=/etc/rancher/k3s/k3s.yaml \
      -n "$1" port-forward --address=127.0.0.1 "service/$2" "8081:$3"
  '';
in
{
  # perlless replaces Perl activation with userborn and an /etc overlay, and
  # fails the build if any Perl store path remains in the closure.
  imports = [
    "${modulesPath}/profiles/minimal.nix"
    "${modulesPath}/profiles/headless.nix"
    "${modulesPath}/profiles/perlless.nix"
  ];
  system.stateVersion = "26.05";
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
    util-linuxMinimal
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
  # No setuid/setgid wrappers: nothing in this appliance needs privilege escalation.
  security.enableWrappers = false;
  microvm = {
    hypervisor = "qemu";
    mem = 4096;
    vcpu = 2;
    storeOnDisk = true;
    storeDiskType = "erofs";
    # -Ededupe forces single-threaded compression in the pinned microvm builder.
    # Retain LZ4HC density and tail packing while allowing parallel construction.
    storeDiskErofsFlags = [ "-zlz4hc" "-Eztailpacking" "-Efragments" ];
    volumes = [{ image = "data.img"; mountPoint = "/var/lib"; size = 102400; fsType = "btrfs"; autoCreate = false; }];
  };
  # One btrfs holds everything. @system is /var/lib; the nested subvolumes for
  # container images, logs and volumes are created by the agent. @snapshots sits
  # beside it and is mounted away from /var/lib so no pod path can reach it.
  fileSystems."/var/lib" = {
    device = lib.mkForce "/dev/disk/by-id/virtio-ca-data";
    fsType = lib.mkForce "btrfs";
    options = [ "subvol=@system" "noatime" "discard=async" ];
    autoResize = true;
    neededForBoot = true;
  };
  fileSystems."/run/ca/snapshots" = {
    device = "/dev/disk/by-id/virtio-ca-data";
    fsType = "btrfs";
    options = [ "subvol=@snapshots" "noatime" "discard=async" "x-mount.mkdir" ];
  };
  services.btrfs.autoScrub = { enable = true; fileSystems = [ "/var/lib" ]; interval = "monthly"; };
  boot.initrd.supportedFilesystems = [ "erofs" "btrfs" ];
  boot.supportedFilesystems = [ "btrfs" ];
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
    # The minor version is pinned: a K3s minor change needs its own tested upgrade
    # path and must never ride in on a nixpkgs bump. Patch releases within the
    # minor may move with the pinned nixpkgs revision.
    package = pkgs.k3s_1_35;
    role = "server";
    nodeName = "containers-anywhere";
    disable = [ "traefik" "servicelb" "metrics-server" "local-storage" ];
    extraFlags = [ "--write-kubeconfig-mode=0600" "--secrets-encryption" ];
    gracefulNodeShutdown.enable = true;
    # Only K3s's own system images are preloaded. Kite, the gateway and the demo
    # are platform workloads applied by the host app and pulled at runtime.
    images = [ config.services.k3s.package.airgap-images ];
    manifests.publication.source = ./publication.yaml;
  };
  # The root filesystem is disposable. Retain the node password across boots,
  # alongside the cluster database, containerd images and local-path volumes.
  systemd.tmpfiles.rules = [
    "d /var/lib/rancher/node 0700 root root -"
    "L+ /etc/rancher/node - - - - /var/lib/rancher/node"
    # The journal lives on the data disk so a forensic trail survives a reboot and
    # can travel with a recovery kit. It is capped below, so it cannot fill the disk.
    "d /var/lib/containers-anywhere/journal 0755 root root -"
    "L+ /var/log/journal - - - - /var/lib/containers-anywhere/journal"
  ];
  services.journald = {
    storage = "persistent";
    extraConfig = ''
      SystemMaxUse=64M
      SystemKeepFree=1G
      SystemMaxFileSize=8M
      MaxRetentionSec=14day
    '';
  };
  # /var/log/journal is linked to the data disk by the agent's init-data step, which
  # runs before the flush. Do not order the flush after tmpfiles: systemd already orders
  # it before tmpfiles, so that is a cycle, and systemd resolves a cycle by dropping one
  # job at random, which can be tmpfiles itself.
  # The management console is whichever Service carries the label below; the
  # appliance does not know which application provides it. A reconnecting local
  # tunnel avoids NodePort/hostNetwork exposure and survives pod replacement.
  # Both HTTP and WebSocket upgrades use the guest agent.
  systemd.services.console-forward = {
    wantedBy = [ "multi-user.target" ];
    after = [ "k3s.service" ];
    # Requires cancels our start job if K3s initially fails (e.g. DHCP has no
    # default route yet). Restart= cannot recover a job that never executed.
    # Start independently and retry until the API and a console Service are available.
    wants = [ "k3s.service" ];
    unitConfig.StartLimitIntervalSec = 0;
    serviceConfig = {
      ExecStart = consoleForward;
      Restart = "always";
      RestartSec = 5;
    };
  };
  systemd.services.containers-agent = {
    wantedBy = [ "multi-user.target" ];
    after = [ "local-fs.target" ];
    path = [ pkgs.systemd config.services.k3s.package pkgs.btrfs-progs pkgs.e2fsprogs ];
    environment.CA_APPLIANCE_VERSION = lib.removeSuffix "\n" (builtins.readFile ../APPLIANCE_VERSION);
    serviceConfig = { ExecStart = "${agent}/bin/guest guest"; Restart = "on-failure"; };
  };
  # Fail closed before anything writes to the data disk: the disk must be empty
  # (a new install) or already carry the format marker this appliance supports.
  systemd.services.data-format = {
    description = "Verify the persistent data disk format";
    wantedBy = [ "sysinit.target" ];
    requiredBy = [ "systemd-tmpfiles-setup.service" "k3s.service" ];
    before = [ "systemd-tmpfiles-setup.service" "systemd-journal-flush.service" "k3s.service" "containers-agent.service" "shutdown.target" ];
    after = [ "local-fs.target" ];
    conflicts = [ "shutdown.target" ];
    unitConfig.DefaultDependencies = false;
    path = [ pkgs.btrfs-progs pkgs.e2fsprogs pkgs.coreutils ];
    environment.CA_APPLIANCE_VERSION = lib.removeSuffix "\n" (builtins.readFile ../APPLIANCE_VERSION);
    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
      UMask = "0077";
      # A refusal must be readable on the console, where the launcher reports it.
      StandardOutput = "journal+console";
      StandardError = "journal+console";
      ExecStart = "${agent}/bin/guest guest init-data";
    };
  };
  # Marks a clean shutdown. It starts before K3s, so on shutdown it stops after K3s has
  # stopped; if the VM dies instead, the marker is still there at the next boot and the
  # agent discards the container image cache, which an unclean stop can leave with
  # empty files.
  systemd.services.ca-session = {
    description = "Record a clean shutdown of the appliance";
    wantedBy = [ "multi-user.target" ];
    before = [ "k3s.service" ];
    after = [ "data-format.service" ];
    unitConfig.RequiresMountsFor = "/var/lib";
    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
      ExecStart = "${pkgs.coreutils}/bin/true";
      ExecStop = "${agent}/bin/guest guest clean-shutdown";
    };
  };
  # The data disk is created once by the host launcher and is never formatted,
  # repartitioned or recreated by any root image.
  assertions = [
    {
      assertion = !config.fileSystems."/var/lib".autoFormat;
      message = "/var/lib must never be auto-formatted: the data disk is created once by the launcher.";
    }
    {
      assertion = !config.systemd.repart.enable && !config.boot.initrd.systemd.repart.enable;
      message = "systemd-repart must stay disabled: no root image may repartition or reformat the data disk.";
    }
  ];
  services.fstrim.enable = true;
}
