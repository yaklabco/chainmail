{
  description = "chainmail: email for AI agents, behind a policy daemon";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      version = self.shortRev or "dirty";
    in
    {
      packages = forAll (pkgs: {
        default = pkgs.buildGoModule {
          pname = "chainmail";
          inherit version;
          src = ./.;
          vendorHash = "sha256-sg/w9NK1xE0t9k8FuFlSLnGoPJyT35XOhSO1N03vlDY=";
          subPackages = [ "cmd/chainmail" ];
          env.CGO_ENABLED = 0;
          ldflags = [ "-s" "-w" "-X github.com/jamesainslie/chainmail/internal/version.Version=${version}" ];
        };
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [ go buf golangci-lint gotestsum age ];
        };
      });
    };
}
