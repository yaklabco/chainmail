# Evaluation-only check that the Nix modules bootstrap writes on NixOS give
# the same config.toml as bootstrap writes elsewhere. Each case is a golden
# pair in internal/setup/testdata/nixos: <case>.nix from RenderAccountNix
# and <case>.toml from NewConfig, both kept by the Go golden tests. The
# module renders the .nix into /etc/chainmail/config.toml, and that file,
# without the daemon table the module adds, must equal the .toml.
{ self, nixpkgs }:
let
  inherit (nixpkgs) lib;
  pkgs = nixpkgs.legacyPackages.x86_64-linux;
  testdata = ../internal/setup/testdata/nixos;

  # The same names as nixCases in internal/setup/render_nix_test.go.
  cases = [
    "gmail-password"
    "icloud-password"
    "hostinger-password"
    "workspace-oauth"
    "workspace-existing-client"
    "quoting"
    "keyword-name"
    "endpoint-override"
  ];

  # rendered is config.toml as the module writes it with the case's .nix
  # imported, parsed and without its daemon table.
  rendered = name:
    let
      sys = lib.nixosSystem {
        system = "x86_64-linux";
        modules = [
          self.nixosModules.default
          (testdata + "/${name}.nix")
          {
            boot.loader.grub.enable = false;
            fileSystems."/" = { device = "none"; fsType = "tmpfs"; };
            system.stateVersion = "26.05";
            services.chainmail = {
              enable = true;
              vaultKeyCredential = "/run/test.cred";
            };
          }
        ];
      };
      toml = builtins.fromTOML (builtins.readFile sys.config.environment.etc."chainmail/config.toml".source);
    in
    removeAttrs toml [ "daemon" ];

  expected = name: builtins.fromTOML (builtins.readFile (testdata + "/${name}.toml"));

  check = name:
    let
      got = rendered name;
      want = expected name;
    in
    lib.assertMsg (got == want) ''
      bootstrap render case ${name}: the module's config.toml differs from ${name}.toml
        from ${name}.nix: ${builtins.toJSON got}
        ${name}.toml:     ${builtins.toJSON want}'';
  # Every golden .nix is a case, so a pair added in Go is not left unchecked.
  goldens = map (lib.removeSuffix ".nix")
    (lib.filter (lib.hasSuffix ".nix") (lib.attrNames (builtins.readDir testdata)));
in
assert lib.assertMsg (lib.sort lib.lessThan cases == goldens)
  "bootstrap render cases ${toString cases} differ from the golden .nix files ${toString goldens}";
assert lib.all check cases;
pkgs.runCommand "chainmail-bootstrap-render" { } "touch $out"
