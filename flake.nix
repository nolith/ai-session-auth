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
              # Go and the tests' golden files, under testdata/.
              ./internal
            ];
          };
          vendorHash = null;

          # no_new_privs needs syscall.AllThreadsSyscall, which refuses in a
          # binary built with cgo; on Linux, net would link cgo.
          env.CGO_ENABLED = 0;

          ldflags = [
            "-s"
            "-w"
          ];

          # geas ssh signs with a YubiKey: it needs OpenSSH's ssh-keygen
          # with FIDO2 support (macOS's /usr/bin/ssh-keygen has none), ykman
          # to find the YubiKey by serial and fido2-token to check it before
          # enrolling. They come first on geas's own PATH; the caller's PATH,
          # saved first, is what a session's harness gets back.
          nativeBuildInputs = [ pkgs.makeWrapper ];
          postInstall = ''
            wrapProgram $out/bin/geas \
              --run 'export GEAS_UNWRAPPED_PATH="''${PATH-}"' \
              --prefix PATH : ${
                lib.makeBinPath [
                  pkgs.openssh
                  pkgs.yubikey-manager
                  pkgs.libfido2
                ]
              }
          '';

          # The tests run real Git, ps, ssh-keygen and /bin/sh.
          nativeCheckInputs = [
            pkgs.gitMinimal
            pkgs.openssh
            pkgs.ps
          ];
          checkFlags = [ "-race" ];
          # The race detector needs cgo on Linux; the installed binary,
          # built before the tests, keeps CGO_ENABLED=0.
          preCheck = ''
            export HOME="$TMPDIR"
            go vet ./...
          ''
          + lib.optionalString pkgs.stdenv.hostPlatform.isLinux ''
            export CGO_ENABLED=1
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
