# Krateo component codegen entry point. `make generate` is the single, uniform way every
# CRD-owning repo regenerates ./crds — go mod tidy + go generate ./... driving the
# build-tagged apis/generate.go controller-gen directive (pinned to go.mod controller-tools).
.PHONY: tidy generate

tidy: ## Ensure all Go imports are satisfied.
	go mod tidy

generate: tidy ## Regenerate CRDs into ./crds.
	go generate ./...
