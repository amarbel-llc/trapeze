{
  description = "Trapeze: a terminal-based AI coding agent (fork of charmbracelet/crush)";

  inputs = {
    # igloo is the amarbel-llc nixpkgs fork. Its overlay carries the patched
    # buildGoApplication (auto-injects -X main.version / -X main.commit) plus
    # mkGoPkgs / mkGoEnv. Importing it directly (not via overlays on upstream
    # nixpkgs) is required — re-applying gomod2nix's upstream overlay would
    # shadow the patched builder. Matches tommy/moxy/maneater.
    igloo.url = "github:amarbel-llc/igloo";
    nixpkgs-master.url = "github:NixOS/nixpkgs/567a49d1913ce81ac6e9582e3553dd90a955875f";
    utils.url = "https://flakehub.com/f/numtide/flake-utils/0.1.102";

    # trapeze's go.mod requires a newer Go patch than the eng-pinned
    # nixpkgs-master ships (it has go_1_26 = 1.26.2; we need >= 1.26.3). This
    # extra pin (nixos-unstable @ 2026-06-06, shipping go_1_26 = 1.26.3)
    # supplies the toolchain for the Go build and devshell only — everything
    # else still resolves through igloo / nixpkgs-master. DROP this once
    # igloo's nixpkgs-master ships go 1.26.4 (amarbel-llc/igloo#40) and source
    # `go` from pkgs-master again — see also trapeze#3.
    nixpkgs-go.url = "github:NixOS/nixpkgs/a799d3e3886da994fa307f817a6bc705ae538eeb";

    # treefmt-nix is the formatter source-of-truth: ./treefmt.nix's
    # `programs.*.enable` resolves each formatter binary, and we consume its
    # generated config file (config.build.configFile). conformist (below) is
    # the actual runner for both `nix fmt` and the read-only check gate.
    treefmt-nix = {
      url = "github:numtide/treefmt-nix";
      inputs.nixpkgs.follows = "igloo";
    };

    # conformist — the linter+formatter multiplexer (treefmt v2 successor; RFC
    # 0001). Runs the formatters from the generated treefmt config plus
    # trapeze's own [linter.*] sections under one `conformist check` gate.
    conformist = {
      url = "github:amarbel-llc/conformist";
      inputs.igloo.follows = "igloo";
      inputs.nixpkgs-master.follows = "nixpkgs-master";
      inputs.utils.follows = "utils";
    };
  };

  outputs =
    {
      self,
      igloo,
      nixpkgs-master,
      nixpkgs-go,
      utils,
      treefmt-nix,
      conformist,
    }:
    let
      # trapeze's release version. Burnt into the binary via the explicit ldflags
      # below (trapeze keeps upstream's internal/version package rather than a
      # main.version var, so the fork's auto-injected -X main.version is a
      # linker no-op here). `just bump-version` rewrites this string.
      trapezeVersion = "0.1.0";
      # shortRev for clean builds, dirtyShortRev ("<sha>-dirty") for dirty
      # working trees, "unknown" as a last-resort fallback.
      trapezeCommit = self.shortRev or self.dirtyShortRev or "unknown";
    in
    utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = import igloo { inherit system; };
        pkgs-master = import nixpkgs-master { inherit system; };
        # go 1.26.3 from the temporary nixpkgs-go pin (go.mod needs >= 1.26.3,
        # which neither igloo nor nixpkgs-master ship yet). This `go` is
        # threaded into BOTH buildGoApplication and the devshell's mkGoEnv
        # (`inherit go`). When retiring the pin (igloo#40 / trapeze#3): drop
        # the nixpkgs-go input, switch this line back to pkgs-master.go_1_26,
        # and the `inherit go` in mkGoEnv keeps working unchanged.
        go = (import nixpkgs-go { inherit system; }).go_1_26;

        # Source for the Go build. trapeze embeds a large, scattered set of
        # non-Go assets via //go:embed (provider.json, migrations/*.sql,
        # builtin/* skills, *.md / *.md.tpl tool prompts, stats/*.{html,css,js,
        # svg}, *.png icons, gitignore/*). The fork's mkGoPkgs helper filters
        # to a Go-only allowlist and drops all of those, breaking the embeds —
        # so instead we exclude a denylist of clearly-non-source paths and keep
        # everything else (the maneater pattern). Edits to those excluded paths
        # don't bust the derivation hash.
        goSrc = pkgs.lib.cleanSourceWith {
          src = ./.;
          filter =
            path: _type:
            let
              base = baseNameOf path;
            in
            !(pkgs.lib.hasInfix "/.tmp/" path)
            && !(pkgs.lib.hasInfix "/.direnv/" path)
            && !(pkgs.lib.hasInfix "/.git/" path)
            && !(pkgs.lib.hasInfix "/build/" path)
            # nix build symlinks (./result, ./result-foo) — matched by basename
            # so a future source file like query_result.go isn't dropped.
            && base != "result"
            && !(pkgs.lib.hasPrefix "result-" base)
            && !(pkgs.lib.hasSuffix "/justfile" path)
            && !(pkgs.lib.hasSuffix "/sweatfile" path)
            && !(pkgs.lib.hasSuffix "/treefmt.nix" path)
            && !(pkgs.lib.hasSuffix "/flake.nix" path)
            && !(pkgs.lib.hasSuffix "/flake.lock" path)
            # Generated outputs that aren't compiled in (regenerated by their
            # own `just build-*` lanes) — excluding them keeps a `just
            # build-schema` / `build-swagger` from busting the build hash.
            # NB: keep internal/swagger/docs.go (it IS compiled in); only the
            # sibling .json/.yaml specs are non-source. Do NOT exclude
            # *.md / *.md.tpl — many are //go:embed'd.
            && !(pkgs.lib.hasSuffix "/schema.json" path)
            && !(pkgs.lib.hasSuffix "/internal/swagger/swagger.json" path)
            && !(pkgs.lib.hasSuffix "/internal/swagger/swagger.yaml" path);
        };

        # Module path (renamed from the upstream fork's
        # github.com/charmbracelet/crush — trapeze#1).
        modulePath = "github.com/amarbel-llc/trapeze";

        ldflags = [
          "-X"
          "${modulePath}/internal/version.Version=${trapezeVersion}"
          "-X"
          "${modulePath}/internal/version.Commit=${trapezeCommit}"
        ];

        # The package set the hermetic go-test / go-vet checks run over: all
        # packages MINUS the two whose tests can't run in the nix build
        # sandbox. internal/agent's VCR tests drive a charm.land/x/vcr recorder
        # against hyper.charm.land (needs network + TRAPEZE_HYPER_API_KEY);
        # internal/shell's TestDispatch_BinaryPassthroughExecutes copies a
        # PATH binary and execs the copy, which fails on the sandbox's
        # store-linked coreutils. Both stay runnable in the devshell via
        # `just test-go` / `just test-agent`. See trapeze#2.
        goTestPackages = "$(go list ./... | grep -vE '/internal/(agent|shell)($|/)')";

        trapeze = pkgs.buildGoApplication {
          pname = "trapeze";
          version = trapezeVersion;
          commit = trapezeCommit;
          src = goSrc;
          pwd = ./.;
          modules = ./gomod2nix.toml;
          subPackages = [ "." ];
          inherit go ldflags;
          CGO_ENABLED = "0";
          GOTOOLCHAIN = "local";
          # The default goCheckHook only tests subPackages (the root package);
          # run the full unit-test surface inside the build sandbox instead,
          # minus the sandbox-incompatible packages (see goTestPackages).
          # HOME is pointed at $TMPDIR because the sandbox default
          # (/homeless-shelter) is read-only and some config tests write a
          # provider cache under $HOME.
          doCheck = true;
          checkPhase = ''
            runHook preCheck
            export HOME="$TMPDIR"
            go test -p $NIX_BUILD_CORES ${goTestPackages}
            runHook postCheck
          '';

          meta = {
            description = "Terminal-based AI coding agent (fork of charmbracelet/crush)";
            homepage = "https://code.linenisgreat.com/trapeze";
            license = pkgs.lib.licenses.mit;
            mainProgram = "trapeze";
          };
        };

        # --- mkTrapeze (modeled after clown's mkCircus) ---------------------
        # Downstream-consumable build function. A consuming flake imports
        # trapeze and calls
        #
        #   trapeze.lib.${system}.mkTrapeze {
        #     plugins  = [ my-plugin-drv ./local-plugin ];
        #     clownBin = clown.packages.${system}.default;  # optional
        #   }
        #
        # and gets back { packages.default, devShells.default, checks } —
        # the same outputs shape clown's mkCircus returns. `plugins` is a
        # list of clown-protocol plugin directories (each carrying a
        # clown.json manifest, RFC-0002); they are baked into the wrapper
        # via TRAPEZE_PLUGIN_DIRS, which internal/pluginhost reads at
        # startup. `clownBin` (a clown package or a path to the binary)
        # is exported as CLOWN_BIN so job producers spawned by trapeze
        # (spinclass, moxy, ...) emit on the job-wakeup channel trapeze's
        # Jobs sidebar watches; without it they stay dormant per the
        # RFC-0009 producer-may-emit contract.
        resolvePluginDirs = plugins: pkgs.lib.concatStringsSep ":" (map toString plugins);

        # The wrapper-script pattern is clown's mkClownBin: a small
        # shell wrapper exporting the baked environment, then exec'ing
        # the Go binary. A caller's environment still works with it:
        # caller-set TRAPEZE_PLUGIN_DIRS entries are unioned in ahead of
        # the baked ones, and a caller-set CLOWN_BIN wins outright.
        mkTrapezeBin =
          {
            pluginDirs ? "",
            clownBin ? null,
          }:
          pkgs.writeShellScriptBin "trapeze" ''
            ${
              pkgs.lib.optionalString (pluginDirs != "") ''
                export TRAPEZE_PLUGIN_DIRS="''${TRAPEZE_PLUGIN_DIRS:+$TRAPEZE_PLUGIN_DIRS:}${pluginDirs}"
              ''
            }${
              pkgs.lib.optionalString (clownBin != null) ''
                export CLOWN_BIN="''${CLOWN_BIN:-${
                  if pkgs.lib.isDerivation clownBin then pkgs.lib.getExe' clownBin "clown" else toString clownBin
                }}"
              ''
            }exec "${trapeze}/bin/trapeze" "$@"
          '';

        mkTrapezePkg =
          {
            plugins ? [ ],
            clownBin ? null,
          }:
          if plugins == [ ] && clownBin == null then
            # Nothing to bake: the raw Go derivation IS the package (and
            # stays the flake's own packages.default below).
            trapeze
          else
            (mkTrapezeBin {
              pluginDirs = resolvePluginDirs plugins;
              inherit clownBin;
            }).overrideAttrs
              (old: {
                passthru = (old.passthru or { }) // {
                  unwrapped = trapeze;
                };
                meta = (old.meta or { }) // {
                  inherit (trapeze.meta) description homepage license;
                  mainProgram = "trapeze";
                };
              });

        mkTrapeze =
          {
            plugins ? [ ],
            clownBin ? null,
          }:
          {
            packages.default = mkTrapezePkg { inherit plugins clownBin; };
            devShells.default = devShell;
            checks = {
              conformist = conformistCheck;
              go-test = trapeze;
              go-lint = goLint;
            };
          };

        # --- hermetic check derivations (moxy pattern) ---------------------
        # The `trapeze` package build already runs the full unit suite in its
        # checkPhase (doCheck = true) and has no postInstall to strip, so it
        # IS the go-test check — `checks.go-test = trapeze` below reuses it
        # rather than materializing an identical second build.
        #
        # No standalone `go vet` check: golangci-lint (below) runs the `govet`
        # analyzer and honors the repo's //nolint directives. A separate raw
        # `go vet ./...` is stricter than the repo's configured linting — it
        # ignores //nolint and so re-flags intentionally-suppressed findings
        # (e.g. internal/csync.Map.JSONSchemaAlias's deliberate value receiver,
        # already //nolint'd for the copylocks check that invopop/jsonschema
        # forces). Letting golangci-lint own vet avoids that redundant,
        # stricter-than-the-repo second pass.

        # golangci-lint as a hermetic check. trapeze's .golangci.yml (v2) uses
        # only built-in analyzers, so it typechecks offline against the
        # buildGoApplication module graph. Overrides `trapeze`'s test checkPhase
        # with the lint run. --config points at the flake's copy of
        # .golangci.yml (the dotfile is filtered out of goSrc). Caches go to
        # $TMPDIR (the sandbox HOME is read-only).
        goLint = trapeze.overrideAttrs (old: {
          pname = "trapeze-golangci-lint";
          nativeBuildInputs = (old.nativeBuildInputs or [ ]) ++ [
            pkgs-master.golangci-lint
          ];
          checkPhase = ''
            runHook preCheck
            export HOME="$TMPDIR"
            export GOLANGCI_LINT_CACHE="$TMPDIR/golangci-lint-cache"
            golangci-lint run --config ${./.golangci.yml} --timeout 10m ./...
            runHook postCheck
          '';
        });

        # --- conformist (formatting + lint) -------------------------------
        treefmtEval = treefmt-nix.lib.evalModule pkgs {
          imports = [ ./treefmt.nix ];
        };

        conformistBin = conformist.packages.${system}.conformist;

        # Take treefmt-nix's generated formatter config and append trapeze's
        # [linter.*] sections. treefmt has no linter table, so a plain append
        # is a valid, order-independent merge.
        #
        # [linter.log-capitalization] ports scripts/check_log_capitalization.sh:
        # slog messages must start with a capital letter. Read-only (no
        # repair-command) — failures are fixed by hand.
        conformistConfig = pkgs.runCommand "conformist-config.toml" { } ''
          cat ${treefmtEval.config.build.configFile} > $out
          cat >> $out <<'EOF'

          [linter.log-capitalization]
          command = "${pkgs.bash}/bin/bash"
          options = [
            "-euc",
            "if grep -nE 'slog\\.(Error|Info|Warn|Debug|Fatal|Print|Println|Printf)\\(\"[a-z]' \"$@\"; then echo 'log messages must start with a capital letter (see lines above)' >&2; exit 1; fi",
            "--",
          ]
          includes = ["*.go"]
          excludes = ["internal/db/*.sql.go", "internal/swagger/**"]
          EOF
        '';

        # `nix fmt` repair entrypoint: run conformist against the merged config.
        # --config-file points at a /nix/store path, and conformist defaults
        # --tree-root to the config's directory, so we MUST pass an explicit
        # --tree-root or it would walk /nix/store.
        conformistFormatter = pkgs.writeShellScriptBin "conformist-fmt" ''
          exec ${conformistBin}/bin/conformist \
            --config-file ${conformistConfig} \
            --tree-root "''${PRJ_ROOT:-$PWD}" \
            "$@"
        '';

        # Read-only gate. Copy the source into the sandbox (the tree must be
        # writable for fix-only formatters' drift check, and we never write
        # back to the real tree), then run `conformist check` rooted at that
        # copy. Non-zero exit fails the build.
        conformistCheck =
          pkgs.runCommand "conformist-check"
            {
              nativeBuildInputs = [ conformistBin ];
            }
            ''
              cp -r ${self} src
              chmod -R u+w src
              cd src
              conformist check \
                --config-file ${conformistConfig} \
                --tree-root .
              touch $out
            '';
        devShell = pkgs-master.mkShell {
          # mkGoEnv puts the gomod2nix-regen `go` wrapper + gomod2nix CLI on
          # PATH and gives `nix develop` the same module graph as `nix build`.
          packages = [
            (pkgs.mkGoEnv {
              pwd = ./.;
              inherit go;
            })
            pkgs-master.gopls
            pkgs-master.gotools
            pkgs-master.golangci-lint
            pkgs-master.delve
            pkgs-master.gofumpt
            pkgs.just
            pkgs.sqlc
            pkgs.svu
            pkgs.git
            pkgs.gh
            conformistBin
          ];
          env.GOEXPERIMENT = "greenteagc";
        };
      in
      {
        packages = {
          default = trapeze;
          inherit trapeze;
        };

        # Downstream build function (see the mkTrapeze comment above).
        lib = {
          inherit mkTrapeze;
        };

        # `nix flake check` (= the `just validate` / pre-merge gate) runs every
        # check below in the build sandbox: conformist (fmt drift +
        # log-capitalization lint), the Go unit tests, and golangci-lint (which
        # includes the govet analyzer, honoring the repo's //nolint).
        checks = {
          conformist = conformistCheck;
          go-test = trapeze;
          go-lint = goLint;
        };

        # `nix fmt` runs conformist in repair mode.
        formatter = conformistFormatter;

        devShells.default = devShell;
      }
    );
}
