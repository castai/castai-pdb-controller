# Makefile for building and pushing a multi-arch Docker image using BuildKit

# Variables
DOCKER_IMAGE_NAME = castai/castai-pdb-controller
DOCKER_IMAGE_TAGS = latest 0.1
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

# Push the Docker image to Docker Hub (informational only)
push:
	@$(foreach tag,$(DOCKER_IMAGE_TAGS),echo "Image pushed to Docker Hub: $(DOCKER_IMAGE_NAME):$(tag)";)

# Clean up the buildx builder
clean:
	-docker buildx rm multiarch-builder || true