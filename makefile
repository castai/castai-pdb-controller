# Makefile for building and pushing a multi-arch Docker image using BuildKit

# Variables — align with Helm default (Artifact Registry castai-hub)
DOCKER_IMAGE_NAME = us-docker.pkg.dev/castai-hub/library/castai-pdb-controller
DOCKER_IMAGE_TAGS = latest 0.5
PLATFORMS = linux/amd64,linux/arm64

# Enable BuildKit
export DOCKER_BUILDKIT = 1

# Default target
all: build push

# Build the Docker image for multiple architectures
build:
	# Check if the multiarch-builder already exists
	@if ! docker buildx inspect multiarch-builder &>/dev/null; then \
		docker buildx create --use --name multiarch-builder; \
	fi
	docker buildx inspect multiarch-builder --bootstrap
	docker buildx build \
		--platform $(PLATFORMS) \
		$(foreach tag,$(DOCKER_IMAGE_TAGS),-t $(DOCKER_IMAGE_NAME):$(tag)) \
		--push \
		.

# Push target is informational (build uses --push to Artifact Registry; authenticate with gcloud/docker first)
push:
	@$(foreach tag,$(DOCKER_IMAGE_TAGS),echo "Image: $(DOCKER_IMAGE_NAME):$(tag)";)

# Clean up the buildx builder
clean:
	-docker buildx rm multiarch-builder || true