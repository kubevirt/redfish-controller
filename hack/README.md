# Hack Directory

This directory contains helper scripts for building, testing, and releasing the kubevirt-redfish project.

## Available Scripts

### `build-and-push-container.sh`

Builds and pushes a container image to a registry using Buildah.

**Usage:**
```bash
./hack/build-and-push-container.sh <tag>
```

**Example:**
```bash
./hack/build-and-push-container.sh v0.2.0
```

**Environment Variables:**
- `REGISTRY`: Container registry (default: `quay.io`)
- `NAMESPACE`: Registry namespace (default: `kubevirt`)
- `IMAGE_NAME`: Image name (default: `redfish-controller`)
- `QUAY_USERNAME`: Registry username (required)
- `QUAY_PASSWORD`: Registry password (required)

### `tag-latest.sh`

Tags an existing image with the 'latest' tag.

**Usage:**
```bash
./hack/tag-latest.sh <commit-sha>
```

**Example:**
```bash
./hack/tag-latest.sh 85b872ea
```

**Environment Variables:**
- `REGISTRY`: Container registry (default: `quay.io`)
- `NAMESPACE`: Registry namespace (default: `kubevirt`)
- `IMAGE_NAME`: Image name (default: `redfish-controller`)
- `QUAY_USERNAME`: Registry username (required)
- `QUAY_PASSWORD`: Registry password (required)

### `test-integration.sh`

Runs integration tests against a real Kubernetes cluster.

**Usage:**
```bash
./hack/test-integration.sh
```

**Environment Variables:**
- `TEST_HOST`: Server host (default: `localhost`)
- `TEST_PORT`: Server port (default: `8443`)
- `TEST_USER`: Username (default: `testuser`)
- `TEST_PASS`: Password (default: `testpass`)

**Requirements:**
- `kubectl` installed and configured
- `jq` installed
- Access to a Kubernetes cluster with KubeVirt installed

## Using with Makefile

The integration test script can be run via the Makefile:

```bash
make test-integration
```

## Using with CI/CD

The build and tag scripts are designed to work with CI/CD systems like GitHub Actions or GitLab CI. They use environment variables for configuration, which keeps the Makefile and CI workflows thin and reusable.

## Customization

All scripts use environment variables for configuration, making them easy to customize for different environments:

```bash
# Example: Use different registry
export REGISTRY="docker.io"
export NAMESPACE="myorg"
export IMAGE_NAME="my-kubevirt-redfish"
./hack/build-and-push-container.sh v1.0.0
``` 