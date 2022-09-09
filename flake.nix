{
  description = "";

  inputs = {
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.simpleFlake {
      inherit self nixpkgs;
      name = "cloudnative-pg";
      shell = { pkgs ? nixpkgs }:
        pkgs.mkShell {
          buildInputs = [
            pkgs.go
            pkgs.gotools
            pkgs.gopls
            pkgs.go-outline
            pkgs.gocode
            pkgs.gopkgs
            pkgs.gocode-gomod
            pkgs.godef
            pkgs.golint
          ];
        };
    };
}
