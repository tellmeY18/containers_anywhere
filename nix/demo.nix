{ pkgs }:
pkgs.dockerTools.buildImage {
  name = "containers-anywhere/demo";
  tag = pkgs.caddy.version;
  copyToRoot = pkgs.buildEnv {
    name = "gateway-demo-root";
    paths = [ pkgs.caddy (pkgs.runCommand "demo-page" { } ''
      mkdir -p $out/www
      cp ${./demo.html} $out/www/index.html
    '') ];
    pathsToLink = [ "/bin" "/www" ];
  };
  config = {
    Entrypoint = [ "/bin/caddy" "file-server" "--listen" ":8080" "--root" "/www" ];
    User = "65532:65532";
  };
}
