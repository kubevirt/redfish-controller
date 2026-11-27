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

Install the `kubevirt-redfish` chart included in this repository. The previously documented public chart endpoints, `https://kubevirt.github.io/charts` and `oci://quay.io/kubevirt/charts/redfish-controller`, are unavailable; see [issue #21](https://github.com/kubevirt/redfish-controller/issues/21).

1. Clone the source and enter the repository root. Git is required for this step.

   ```bash
   git clone https://github.com/kubevirt/redfish-controller.git
   cd redfish-controller
   ```

   This installs the chart from your source checkout, rather than a published chart release. To use a specific Git revision, run `git checkout <revision>` before continuing. Run all chart commands below from the repository root.

2. Review and save the chart values for editing.

   ```bash
   helm show values ./helm
   helm show values ./helm > my-values.yaml
   ```

   Edit `my-values.yaml` for your environment, including authentication, chassis namespaces, and the OpenShift route. Set `global.namespace` to the installation namespace if you change it. Choose an available container image tag in `global.image.tag` from the [container repository](https://quay.io/repository/kubevirt/redfish-controller?tab=tags); selecting a Git revision does not change the image tag automatically.

3. Install the chart with your custom values.

   ```bash
   helm install kubevirt-redfish ./helm \
     --namespace kubevirt-redfish \
     --create-namespace \
     -f my-values.yaml
   ```

   You can also override values inline. Replace `<image-tag>` with your chosen container image tag:

   ```bash
   helm install kubevirt-redfish ./helm \
     --namespace kubevirt-redfish \
     --create-namespace \
     -f my-values.yaml \
     --set-string global.image.tag='<image-tag>' \
     --set service.type=LoadBalancer
   ```

For upgrades, see the [chart upgrade instructions](helm/README.md#upgrading).

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
