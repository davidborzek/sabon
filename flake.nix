{
  description = "Dev shell for sabon";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = {
    self,
    nixpkgs,
  }: let
    systems = [
      "x86_64-linux"
      "aarch64-linux"
      "aarch64-darwin"
    ];

    forAllSystems = nixpkgs.lib.genAttrs systems;
    pkgsFor = system: nixpkgs.legacyPackages.${system};

  in {
    devShells = forAllSystems (
      system:
      let
        pkgs = pkgsFor system;
      in {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go_1_26
            gopls
            gotools
            golangci-lint
          ];
        };
      }
    );
  };
}
