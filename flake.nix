{
  description = "GEAS Enforces Agent Scopes: temporary credentials for AI agents, under your own identity";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs =
    { self, nixpkgs }:
    let
      inherit (nixpkgs) lib;
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
      forAllSystems = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      treefmtFor =
        pkgs:
        pkgs.treefmt.withConfig {
          runtimeInputs = [
            pkgs.go
            pkgs.nixfmt
          ];
          settings = lib.importTOML ./treefmt.toml;
        };
    in
    {
      packages = forAllSystems (pkgs: {
        default = self.packages.${pkgs.stdenv.hostPlatform.system}.geas;
        geas = pkgs.buildGoModule {
          pname = "geas";
          version = self.shortRev or self.dirtyShortRev or "dev";

          src = lib.fileset.toSource {
            root = ./.;
            fileset = lib.fileset.unions [
              ./go.mod
              ./config.example.json
              (lib.fileset.fileFilter (file: file.hasExt "go") ./.)
              # The tests' golden files.
              (lib.fileset.fileFilter (file: file.hasExt "yml" || file.hasExt "json") ./internal)
            ];
          };
          vendorHash = null;

          ldflags = [
            "-s"
            "-w"
          ];

          # The tests run real Git, ps, /bin/sh and UNIX sockets.
          nativeCheckInputs = [
            pkgs.gitMinimal
            pkgs.ps
          ];
          checkFlags = [ "-race" ];
          preCheck = ''
            export HOME="$TMPDIR"
            go vet ./...
          '';

          meta = {
            description = "Launch AI agents with short-lived GitHub and GitLab credentials";
            homepage = "https://github.com/nolith/geas";
            mainProgram = "geas";
            platforms = lib.platforms.linux ++ lib.platforms.darwin;
          };
        };
      });

      formatter = forAllSystems treefmtFor;

      checks = forAllSystems (pkgs: {
        build = self.packages.${pkgs.stdenv.hostPlatform.system}.geas;
        formatting = (treefmtFor pkgs).check self;
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          inputsFrom = [ self.packages.${pkgs.stdenv.hostPlatform.system}.geas ];
          packages = [
            pkgs.gopls
            pkgs.gh
            pkgs.glab
            pkgs.nixfmt
            # Plain treefmt, reading treefmt.toml from the working tree.
            pkgs.treefmt
          ];
        };
      });
    };
}
