{ pkgs }:
let
  arch = if pkgs.stdenv.hostPlatform.isAarch64 then "arm64" else "amd64";
  binary = pkgs.stdenvNoCC.mkDerivation {
    pname = "kite";
    version = "0.16.0";
    src = pkgs.fetchurl {
      url = "https://github.com/kite-org/kite/releases/download/v0.16.0/kite-${arch}-v0.16.0.tar.gz";
      sha256 =
        if arch == "arm64"
        then "d13d479529b1a1aef871567d6408310406c9e37ff83267b09e7f23209d9160bb"
        else "8ddcda4b3e770a778edf88e2cc95a1f5a9f36343a20c7c62ce00a91b7049b17a";
    };
    sourceRoot = ".";
    dontStrip = true;
    installPhase = ''
      mkdir -p $out/bin
      install -m755 bin/kite-${arch} $out/bin/kite
    '';
  };
in
pkgs.dockerTools.buildImage {
  name = "containers-anywhere/kite";
  tag = "0.16.0";
  copyToRoot = pkgs.buildEnv {
    name = "kite-root";
    paths = [ binary pkgs.cacert ];
    pathsToLink = [ "/bin" "/etc/ssl/certs" ];
  };
  config = {
    Entrypoint = [ "/bin/kite" ];
    WorkingDir = "/data";
    Env = [ "SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt" ];
  };
}
