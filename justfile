set shell := ["bash", "-cu"]

default:
    @just --list

# Serve the proposal page locally
docs port="8000":
    python3 -m http.server {{port}} --directory docs

# Format Terraform once modules exist
fmt:
    terraform fmt -recursive

# Validate every module
validate:
    for d in modules/*/; do terraform -chdir="$d" init -backend=false -input=false >/dev/null && terraform -chdir="$d" validate; done
