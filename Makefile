IMAGE_NAME=ghcr.io/hydraide/hydraideserver
IMAGE_TAG=$(IMAGE_TAG)

# load the .env file to get the GitHub username and token
include .env
export $(shell sed 's/=.*//' .env)

LIVE_ENV=live
TEST_ENV=test

# Docker build args
BUILD_ARGS=--build-arg GIT_USERNAME=$(GITHUB_USERNAME) \
           --build-arg GIT_EMAIL=$(GITHUB_EMAIL) \

DOCKER_BUILDKIT=1

# Docker build and push
.PHONY: build push build-push all

# Build the Docker image with the specified tag
build:
	echo $(GITHUB_TOKEN) > .git_token_file
	docker build --secret id=git_token,src=.git_token_file $(BUILD_ARGS) -f Dockerfile -t $(IMAGE_NAME):$(IMAGE_TAG) .
	rm .git_token_file

# Push the Docker image to GitHub Container Registry
push:
	echo $(GITHUB_CONTAINER_TOKEN) | docker login ghcr.io -u $(GITHUB_USERNAME) --password-stdin
	docker tag $(IMAGE_NAME):$(IMAGE_TAG) $(IMAGE_NAME):latest
	docker push $(IMAGE_NAME):$(IMAGE_TAG)
	docker push $(IMAGE_NAME):latest

# Build the Docker image with both versioned tag and latest tag
build-push: build push

# Build and push with the specific tag
all: build push