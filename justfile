# The hermetic gate is `nix flake check` (via `validate`): it runs conformist
# (fmt + log-capitalization lint), go-test, and go-lint (which includes govet)
# in the build sandbox. `nix flake check` is the source of truth and what
# spinclass's pre-merge hook runs.
#
# CAVEAT (trapeze#3, blocked on amarbel-llc/igloo#40): a dependency
# (charm.land/fantasy) requires go >= 1.26.4, which nixpkgs has not packaged
# yet — the flake's go is 1.26.3 via the temporary nixpkgs-go pin. The nix
# *sandbox* build tolerates this, so the gate (validate/build/test via the
# -nix recipes) is green. But the *devshell* `go` (1.26.3) refuses the
# fantasy requirement, so the host-side `build-go` / `test-go` / `lint-go` /
# `run-dev` loops DO NOT WORK until go 1.26.4 is packaged. Use the `-nix`
# lanes meanwhile.
#
# validate, build, and test — the full nix gate
default: validate build test

# Current nix system double (e.g. aarch64-darwin), used to address
# `.#checks.${system}.*` outputs. Evaluated once per `just` invocation.
system := `nix eval --raw --impure --expr 'builtins.currentSystem'`

# === aggregates ===

validate: validate-nix

build: build-nix

test: test-go-nix test-lint-nix

# === pre-build ===

# Schema-validate the flake and build every checks.${system}.* output
# (conformist, go-test, go-vet, go-lint). This is the pre-merge gate.
#
# schema-validate the flake and build every checks.${system}.* output
[group('pre-build')]
validate-nix:
  nix flake check --show-trace --print-build-logs

# Read-only lint+format gate: build the sandboxed conformist check derivation,
# which runs every formatter (drift check) plus the log-capitalization linter
# and exits non-zero on any finding, with no working-tree side effects.
# Write-mode counterpart: codemod-fmt (`nix fmt`).
#
# check formatting and the log-capitalization linter
[group('pre-build')]
lint-fmt:
  nix build --print-build-logs --no-link ".#checks.{{system}}.conformist"

# Go static analysis via golangci-lint in the devshell (fast loop). Config:
# ./.golangci.yml. The hermetic equivalent (go-lint) runs inside `nix flake
# check`.
#
# run golangci-lint in the devshell
[group('pre-build')]
lint-go:
  GOLANGCI_LINT_CACHE='{{ justfile_directory() }}/.tmp/golangci-lint' golangci-lint run --config .golangci.yml --timeout 10m ./...

# === build ===

[group('build')]
build-nix:
  nix build --show-trace --print-build-logs

# Output binary: ./trapeze.
#
# fast devshell build (no nix sandbox)
[group('build')]
build-go:
  go build -o trapeze .

# Regenerate the sqlc query/model code under internal/db from
# internal/db/{migrations,sql}. Config: ./sqlc.yaml.
#
# regenerate the sqlc query/model code under internal/db
[group('build')]
build-sqlc:
  sqlc generate

# regenerate schema.json (the config JSON schema) from the running binary
[group('build')]
build-schema:
  go run . schema > schema.json

# Regenerate the OpenAPI spec under internal/swagger from the swag annotations
# on the server handlers + main.go.
#
# regenerate the OpenAPI spec under internal/swagger
[group('build')]
build-swagger:
  go run github.com/swaggo/swag/cmd/swag@v1.16.6 init --generalInfo main.go --dir . --output internal/swagger --packageName swagger --parseDependency --parseInternal --parseDepth 5

# regenerate the embedded Hyper provider.json (internal/agent/hyper)
[group('build')]
build-hyper:
  go generate ./internal/agent/hyper/...

# === post-build ===

# Go unit tests in the nix sandbox (the buildGoApplication checkPhase runs
# `go test` over everything except ./internal/agent). --rebuild forces
# re-execution even when the store path is cached; -L streams test logs.
#
# run the Go unit tests in the nix sandbox
[group('post-build')]
test-go-nix:
  nix build -L --rebuild --no-link ".#checks.{{system}}.go-test"

# run golangci-lint in the nix sandbox
[group('post-build')]
test-lint-nix:
  nix build -L --no-link ".#checks.{{system}}.go-lint"

# Fast devshell Go test loop (-race), excluding the VCR agent suite (which
# needs network + TRAPEZE_HYPER_API_KEY — see test-agent). Unlike the nix gate
# (which also excludes internal/shell, whose binary-passthrough test fails on
# the sandbox's store-linked coreutils), this devshell lane KEEPS
# internal/shell — that test runs fine against the host's real PATH.
# `just test-go ./internal/config/` narrows to one package.
#
# run the devshell Go test loop (-race), excluding the agent suite
[group('post-build')]
test-go pkgs='./...':
  go test -race -failfast $(go list {{pkgs}} | grep -vE '/internal/agent($|/)')

# Run the VCR-cassette agent suite (internal/agent) in the devshell. Replays
# cassettes in internal/agent/testdata; needs TRAPEZE_HYPER_API_KEY only when
# re-recording (see test-record). Excluded from the nix gate; see
# trapeze#agent-test-lane.
#
# run the VCR-cassette agent suite in the devshell
[group('post-build')]
test-agent run='.':
  go test -run '{{run}}' ./internal/agent/...

# === codemod ===

# Format the whole tree in place via conformist (`nix fmt`): goimports +
# gofumpt, nixfmt, shfmt, prettier. Read-only counterpart: lint-fmt.
#
# format the whole tree in place via `nix fmt`
[group('codemod')]
codemod-fmt *args:
  nix fmt {{args}}

# === maintenance ===

# Regenerate gomod2nix.toml. Run after changing go.mod / go.sum so the nix
# build's vendored module set stays in sync. Prefers the devshell's gomod2nix;
# falls back to the igloo-pinned package via `nix run` so it also works during
# bootstrap, before the new devshell is active.
#
# regenerate gomod2nix.toml
[group('maintenance')]
update-gomod2nix:
  #!/usr/bin/env bash
  set -euo pipefail
  if command -v gomod2nix >/dev/null 2>&1; then
    gomod2nix
  else
    nix run github:amarbel-llc/igloo#gomod2nix
  fi

# Re-record the VCR cassettes for the agent suite (hits hyper.charm.land — set
# TRAPEZE_HYPER_API_KEY first). Mirrors the old Taskfile `test:record`.
#
# re-record the VCR cassettes for the agent suite
[group('maintenance')]
update-cassettes:
  rm -rf internal/agent/testdata
  go test -v -count=1 -timeout=1h ./internal/agent

# bump the trapezeVersion string in flake.nix
[group('maintenance')]
bump-version new_version:
  #!/usr/bin/env bash
  set -euo pipefail
  current=$(grep 'trapezeVersion = "' flake.nix | head -1 | sed 's/.*"\(.*\)".*/\1/')
  if [[ "$current" == "{{new_version}}" ]]; then
    echo "already at {{new_version}}" >&2
    exit 0
  fi
  sed -i.bak 's/trapezeVersion = "'"$current"'"/trapezeVersion = "{{new_version}}"/' flake.nix && rm flake.nix.bak
  echo "$current → {{new_version}}"

# Create a signed git tag for the current trapezeVersion and push it to origin.
# Release artifacts are built by goreleaser (.goreleaser.yml) off the tag.
#
# create a signed git tag for the current trapezeVersion and push it
[group('maintenance')]
deploy-tag:
  #!/usr/bin/env bash
  set -euo pipefail
  version=$(grep 'trapezeVersion = "' flake.nix | head -1 | sed 's/.*"\(.*\)".*/\1/')
  tag="v${version}"
  if git rev-parse "$tag" >/dev/null 2>&1; then
    echo "tag $tag already exists" >&2
    exit 1
  fi
  git tag -s "$tag" -m "Release $tag"
  echo "created tag $tag"
  git push origin "$tag"
  echo "pushed tag $tag"

# === run ===

# Pass args after `--`.
#
# run the freshly-built binary (devshell, no nix)
[group('run')]
run-dev *args:
  go run . {{args}}

# run with pprof profiling enabled (serves localhost:6060)
[group('run')]
run-profile *args:
  TRAPEZE_PROFILE=true go run . {{args}}
