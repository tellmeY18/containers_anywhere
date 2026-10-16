{ pkgs }:
let
  image = pkgs.dockerTools.buildImage {
    name = "containers-anywhere/traefik";
    tag = pkgs.traefik.version;
    copyToRoot = pkgs.buildEnv {
      name = "traefik-root";
      paths = [ pkgs.traefik pkgs.cacert ];
      pathsToLink = [ "/bin" "/etc/ssl/certs" ];
    };
    config = {
      Entrypoint = [ "/bin/traefik" ];
      User = "65532:65532";
      Env = [ "SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt" ];
    };
  };
  manifest = pkgs.writeText "traefik.yaml" (builtins.replaceStrings
    [ "@TRAEFIK_VERSION@" ] [ pkgs.traefik.version ]
    (builtins.readFile ./traefik.yaml));
in
{ inherit image manifest; }
