# KubeVirt Redfish

> [!WARNING]
> The repository recently moved under Kubevirt and the README has
> not been fully updated yet. Some of the links might lead to the
> personal repository of the original maintainer or not existing
> kubevirt urls.

[![CI/CD](https://github.com/kubevirt/redfish-controller/actions/workflows/ci.yml/badge.svg)](https://github.com/kubevirt/redfish-controller/actions)
[![Go](https://img.shields.io/badge/go-1.24+-blue.svg)](https://golang.org/dl/)
[![Coverage](https://codecov.io/gh/kubevirt/redfish-controller/branch/main/graph/badge.svg)](https://codecov.io/gh/kubevirt/redfish-controller)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Container](https://img.shields.io/badge/container-quay.io-red.svg)](https://quay.io/repository/kubevirt/redfish-controller)
[![Helm](https://img.shields.io/badge/helm-oci-blue.svg)](https://quay.io/repository/kubevirt/charts/redfish-controller)
[![Platform](https://img.shields.io/badge/platform-OpenShift-red.svg)](https://docs.openshift.com/container-platform/latest/virt/about-virt.html)

A Redfish-compatible API server for KubeVirt/OpenShift Virtualization that enables out-of-band management of virtual machines through standard Redfish protocols.

## Table of Contents

- [Overview](#overview)
- [Features](#features)
- [Architecture](#architecture)
- [Installation](#installation)
  - [Prerequisites](#prerequisites)
  - [Quick Start (Helm)](#installation-quickstart-via-helm)
  - [Verification](#verification)
- [Configuration](#configuration)
- [Usage](#usage)
- [Contributing](#contributing)
- [License](#license)

## Overview

KubeVirt Redfish bridges the gap between traditional virtualization management tools and cloud-native KubeVirt environments by providing a Redfish-compliant API interface. This enables existing datacenter management tools, IPMI utilities, and automation scripts to manage KubeVirt virtual machines using familiar Redfish protocols.

## Features

- **Redfish API Compatibility** - Standard Redfish protocol support for VM management
- **KubeVirt Integration** - Native integration with KubeVirt/OpenShift Virtualization
- **Authentication & Authorization** - Kubernetes RBAC integration
- **Multi-Architecture Support** - AMD64 and ARM64 container images
- **Cloud-Native** - Kubernetes-native deployment and configuration
- **Observability** - Comprehensive logging and metrics
- **High Availability** - Scalable and resilient architecture

## Architecture

```
┌─────────────────┐    ┌──────────────────┐    ┌─────────────────┐
│   Redfish       │    │   KubeVirt       │    │   KubeVirt      │
│   Clients       │───▶│   Redfish        │───▶│   VMs           │
│   (ACM, etc.)   │    │   API Server     │    │   (OCP/k8s)     │
└─────────────────┘    └──────────────────┘    └─────────────────┘
```

## Installation

### Prerequisites

- Kubernetes cluster with KubeVirt or OpenShift with OpenShift Virtualization
- **KubeVirt must have the RebootPolicy API enabled for Boot override Once support to work!** (Alpha in kubevirt 1.8 / OCP 4.22)
- Helm 3.12+ 
- `kubectl` or `oc` CLI access

## Installation QuickStart (via Helm)

The `kubevirt-redfish` project uses [Quay.io](https://quay.io/) to store both the [container](https://quay.io/repository/kubevirt/redfish-controller?tab=tags) and [Helm](https://quay.io/repository/kubevirt/charts/kubevirt-redfish?tab=tags) chart artifacts. Previous instructions were to written to support a direct OCI installation (i.e. `helm pull oci://quay.io/kubevirt/charts/redfish-controller --version 0.2.1`), however I would suggest you follow the instructions below to install `kubevirt-redfish`. Reserve any direct OCI chart testing for development purposes only. There is also an [operator](https://github.com/kubevirt/redfish-controller-operator) that can be leveraged as well, but please wait until release v0.3.0 for these projects to work seamlessly. For now, follow the instructions below for the most recent installation proceedures.

(*Updated: 25/08/25 at 11:35UTC)

1. Add the Helm repository for the most recent Chart for this project (`kubevirt-redfish`).

   ```bash
   helm repo add kubevirt https://kubevirt.github.io/charts
   helm repo update
   ```

   You should then see the Helm chart when performing a `helm list`.

   ```bash
   ❯ helm search repo kubevirt
   NAME                         CHART VERSION   APP VERSION     DESCRIPTION
   kubevirt/redfish-controller     0.2.3           v0.2.3-f87ff92  Custom kubevirt-redfish chart with enhanced fea...
   kubevirt/vms-on-ocpv          0.1.1           v0.1.1-ff18cb3  A Helm chart that deploys Virtual Machines on O...
   ```

2. You can then review the example `values.yaml` included with the Helm chart by using the following command.
   ```bash
   helm show values kubevirt/redfish-controller --version 0.2.3
   ```

   NOTE: If you prefer to have a cleaned version of the `values.yaml` (i.e. without comments or line breaks) you can run the following.
   ```bash
   helm show values kubevirt/redfish-controller --version 0.2.3 | sed 's/\s*#.*$//' | grep -v '^\s*$'
   ```

3. To save the `values.yaml` to your your local environment for direct editing, just redirect the output to a YAML file.
   ```bash
   helm show values kubevirt/redfish-controller --version 0.2.3 > my-values.yaml
   ```

4. You can also save the chart locally by issuing the following command.
   ```bash
   # Download the Chart locally:
   helm pull kubevirt/redfish-controller --version 0.2.3
   
   # Explode the tar file:
   tar -xzf kubevirt-redfish-0.2.3.tgz

   # Make a copy of the sample values.yaml manifest:
   cp kubevirt-redfish/values.yaml my-values.yaml
   ```

5. Now you can install the Chart using your own custom values.yaml:
   ```bash
   helm install kubevirt-redfish kubevirt/redfish-controller --version 0.2.3 \
     --namespace kubevirt-redfish \
     --create-namespace \
     -f my-values.yaml
   ```

6. You can also ***optionally*** use inline edits during the `helm install` command.
   ```bash
   helm install kubevirt-redfish kubevirt/redfish-controller \
     --set image.tag=v0.2.3 \
     --set service.type=LoadBalancer \
     --namespace kubevirt-redfish \
     --create-namespace
   ```

### Verification

You can verify the installation with the following commands.
```bash
oc get pods -n kubevirt-redfish
helm list -n kubevirt-redfish
```

## Configuration

Key configuration options available in `values.yaml`:

```yaml
# =============================================================================
# ROUTE CONFIGURATION (OpenShift)
# =============================================================================

route:
  enabled: true
  host: "kubevirt-redfish-namespace-0.clustername.apps.example.com"
  tls:
    termination: "edge"
    insecureEdgeTerminationPolicy: "Redirect"

# =============================================================================
# CHASSIS CONFIGURATION
# =============================================================================

chassis:
  - name: "chassis-0"
    namespace: "namespace-0"
    service_account: "kubevirt-redfish"
    description: "My KVM cluster with test VMs"
  - name: "chassis-1"
    namespace: "namespace-1"
    service_account: "kubevirt-redfish"
    description: "My KVM cluster with test VMs"

# =============================================================================
# AUTHENTICATION CONFIGURATION
# =============================================================================

authentication:
  users:
    # Plain-text password (simple, but stored in clear text)
    - username: "admin"
      password: "changeme"  # CHANGE THIS PASSWORD!
      chassis: ["chassis-0"]

    # Bcrypt-hashed password (recommended for production)
    - username: "user"
      password:
        hash: "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
      chassis: ["chassis-1"]

# =============================================================================
# DATAVOLUME CONFIGURATION
# =============================================================================

datavolume:
  storage_size: "3Gi"
  allow_insecure_tls: true
  storage_class: "lvms-vg1"
  vm_update_timeout: "2m"
  iso_download_timeout: "30m"
```

### Generating bcrypt password hashes

User passwords can be stored as bcrypt hashes instead of plain text. To generate a hash, use any of the following commands on a standard Linux machine:

```bash
# Using htpasswd (Apache utilities)
htpasswd -nbB "" 'YourPassword' | cut -d: -f2

# Using Python 3
python3 -c "import bcrypt; print(bcrypt.hashpw(b'YourPassword', bcrypt.gensalt()).decode())"

# Using mkpasswd (from the whois package on Debian/Ubuntu)
mkpasswd --method=bcrypt 'YourPassword'
```

Place the resulting hash (starting with `$2a$`, `$2b$`, or `$2y$`) into the config:

```yaml
password:
  hash: "$2a$10$..."
```

## Usage

Once deployed, the Redfish API will be available at the service (k8s) or route (OCP) endpoint. In OpenShift, this will be the route endpoint (which the Helm Chart will use, if configured).

**Example** - Query the root URL without authentication (this is defined as per the Redfish specification)

```
curl -k https://kubevirt-redfish-{namespace}.apps.{cluster_name}.{domain_name}/redfish/v1/
```

**Example** - Return a list of managed systems (VMs)

```
curl -k -u user:pass https://kubevirt-redfish-{namespace}.apps.{cluster_name}.{domain_name}/redfish/v1/Systems
```

**Example** - Powering on a VM

```
curl -kX POST -u user:pass https://kubevirt-redfish-{namespace}.apps.{cluster_name}.{domain_name}/redfish/v1/Systems/{vm-id}/Actions/ComputerSystem.Reset \
  -H "Content-Type: application/json" \
  -d '{"ResetType": "On"}'
```

**Example** - Returned output from powering on a VM
```json
{
  "@odata.context": "/redfish/v1/$metadata#ActionResponse.ActionResponse",
  "@odata.id": "/redfish/v1/Systems/{vm-id}/Actions/ComputerSystem.Reset",
  "@odata.type": "#ActionResponse.v1_0_0.ActionResponse",
  "Id": "Reset",
  "Messages": [
    {
      "Message": "Power action On executed successfully"
    }
  ],
  "Name": "Reset Action",
  "Status": {
    "Health": "OK",
    "State": "Completed"
  }
}
```

## Troubleshooting

The `kubevirt-redfish` project has a fairly robust logging system that can be used for troubleshooting client-to-server communication. If you need to troubleshoot specific Redfish client-to-server requests, you can do something like the following below.

```shell
# Search for all logs with a specific correlation ID
oc logs -n kubevirt-redfish deployment/kubevirt-redfish | grep "298d7450ac8695c7"

# This will produce some additional details to help you understand more details about your client/server communication (this is just an empty/unauthenicated request from OpenShift's Advanced Cluster Manager)
2025/08/09 00:22:47 {"timestamp":"2025-08-09T00:22:47.510Z","level":"INFO","message":"Redfish API request received","correlation_id":"298d7450ac8695c7","fields":{"correlation_id":"298d7450ac8695c7","method":"GET","operation":"request","path":"/redfish/v1/","status":"started","user":"unknown"}}
2025/08/09 00:22:47 {"timestamp":"2025-08-09T00:22:47.510Z","level":"DEBUG","message":"Authentication attempt","correlation_id":"298d7450ac8695c7","fields":{"correlation_id":"298d7450ac8695c7","method":"GET","operation":"authentication","path":"/redfish/v1/","remote_addr":"10.128.0.2:45814","user":"unknown","user_agent":"kube-probe/1.32"}}
2025/08/09 00:22:47 {"timestamp":"2025-08-09T00:22:47.510Z","level":"INFO","message":"Redfish API response completed","correlation_id":"298d7450ac8695c7","fields":{"correlation_id":"298d7450ac8695c7","duration":"164.438µs","method":"GET","operation":"response","path":"/redfish/v1/","status":"completed","status_code":200,"user":"unknown"}}
```



## Contributing

We welcome contributions! Please see our [Contributing Guide](CONTRIBUTING.md) for details.

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Add tests
5. Submit a pull request

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

---

**Built for the KubeVirt, Kubernetes, and OpenShift communities**
