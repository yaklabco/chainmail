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
          vendorHash = "sha256-yBp6s/J9fEx2GM1W3Qb08TV893tE4CcTKxIfQPg9wW0=";
          subPackages = [ "cmd/chainmail" ];
          env.CGO_ENABLED = 0;
          ldflags = [ "-s" "-w" "-X github.com/jamesainslie/chainmail/internal/version.Version=${version}" ];
          nativeBuildInputs = [ pkgs.installShellFiles ];
          # Generating the scripts runs the built binary, which a cross build cannot.
          postInstall = pkgs.lib.optionalString (pkgs.stdenv.buildPlatform.canExecute pkgs.stdenv.hostPlatform) ''
            installShellCompletion --cmd chainmail \
              --bash <($out/bin/chainmail completion bash) \
              --zsh <($out/bin/chainmail completion zsh) \
              --fish <($out/bin/chainmail completion fish)
            $out/bin/chainmail man > chainmail.1
            installManPage chainmail.1
          '';
        };
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [ go buf golangci-lint gotestsum age ];
        };
      });

      nixosModules.default = import ./nix/module.nix { inherit self; };

      checks.x86_64-linux.nixos-module = import ./nix/test.nix {
        inherit self;
        pkgs = nixpkgs.legacyPackages.x86_64-linux;
      };
      checks.x86_64-linux.nixos-module-assertions = import ./nix/assertions.nix { inherit self nixpkgs; };
    };
}
