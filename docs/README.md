# Quickstart Guide (OpenShift)

This guide is intended to provide an end-to-end demonstration of an OpenShift `ClusterInstance` (i.e. [SiteConfig Operator](https://docs.redhat.com/en/documentation/red_hat_advanced_cluster_management_for_kubernetes/2.14/html/multicluster_engine_operator_with_red_hat_advanced_cluster_management/siteconfig-intro)) with the `kubevirt-redfish` project.

## Prepare the Namespace

1. Use the following commands to prepare the example `Namespace`. I will be using a namespace called `jinkit-kvm`, and I would suggest using the same with minimal changes - at least until you understand the project and know how to configure the application for your specific environment. I want to give you a "working" example to start with (as close as possible), and let you reverse engineer the demonstration as need for your intended environment.

   ```bash
   oc new-project jinkit-kvm
   ```

2. Next I want to annotate the VM with a finalizer, which will prevent an automatic deletion (which will absolutely trigger if you remove the `ClusterInstance`).

   ```bash
   oc patch namespace jinkit-kvm --type=merge -p '{"metadata":{"finalizers":["kubernetes.io/metadata-controller","namespace-protection.kubernetes.io/delete-protection"]}}'
   ```

## Prepare Helm and Helm Repository

### Download Helm Utility

1. Log into the OpenShift WebUI. Once logged in, you can navigate to the top right-hand corner of the Web UI, look for a question mark (?) and select Command Line Tools.

2. On the next page, look for a title that says **"helm - Helm 3 CLI"** and click on the link which says **"Download Helm"**.

3. This will open a new web link to [mirror.openshift.com](https://mirror.openshift.com/pub/openshift-v4/clients/helm/latest). You can select your version of Helm, unpack it, and move it to a location as defined in your `$PATH` (typically `/usr/local/bin` works).

### Add the Helm Repository

1. Add the following Helm repository using the command below. This Charts repository is hosted via [GitHub pages](http://github.com/v1k0d3n/charts).

   ```bash
   helm repo add v1k0d3n https://v1k0d3n.github.io/charts
   helm repo update v1k0d3n
   ```

2. Now you can view the versions of the Helm chart with the following command. Be aware of the `appVersion` tag and commit, which matches tags and commits within this GitHub repository (i.e. `v0.2.3-f87ff92`).

   ```bash
   helm search repo v1k0d3n/kubevirt-redfish --versions
   ```

   Example:
   ```bash
   ❯ helm search repo v1k0d3n/kubevirt-redfish --versions
   NAME                    	CHART VERSION	APP VERSION   	DESCRIPTION
   v1k0d3n/kubevirt-redfish	0.2.3        	v0.2.3-f87ff92	Custom kubevirt-redfish chart with enhanced fea...
   ```

## Step 1: Install Virtual Machines (via Helm)

1. Using the `v1k0d3n` Helm repository, you will need to deploy the VMs using the sample Helm chart called `v1k0d3n/vms-on-ocpv`. To do this, create a custom values.yaml called `custom-values-vms.yaml` using the following command.

   ```bash
   helm show values v1k0d3n/vms-on-ocpv --version 0.1.0 | sed 's/\s*#.*$//' | grep -v '^\s*$' > custom-values-vms.yaml
   ```

2. There's really only **ONE OVERRIDE** that's ***required*** in the `custom-values-vms.yaml` file. You can also alternatively, modify the deployment in real-time by using the command below. Look for the field `VirtualMachine.resources.network.networkName`, and change this to the [`NetworkAttachmentDefinition`](https://docs.redhat.com/en/documentation/openshift_container_platform/4.18/html/multiple_networks/primary-networks#about-primary-nwt-nad) (NAD) you're using for your virtual machines network connectivity. I am purposely ***not*** using a Pod network as the default network with these VMs. I am only using a secondary network. You can technically leave all other fields "as is".

   **Example:**
   ```bash
   helm upgrade --install vms-jinkit-kvm v1k0d3n/vms-on-ocpv \
     --namespace jinkit-kvm \
     --set VirtualMachine.resources.network.networkName=default/vlan3
   ```

   *I will provide the sample `custom-values-vms.yaml` I use in the [examples folder](./exmaples/).*

3. Once this is complete, verify your VMs have been deployed (they should default to a "Halted" state).

   ```bash
   ❯ oc get vms -n jinkit-vms
   NAME                AGE     STATUS    READY
   ztp-jinkit-kvm-00   4h40m   Stopped   True
   ztp-jinkit-kvm-01   4h40m   Stopped   True
   ztp-jinkit-kvm-02   4h40m   Stopped   True
   ```

## Step 2: Install kubevirt-redfish (via Helm)

1. Again using the `v1k0d3n` Helm repository, you can deploy the `kubevirt-redfish` using the Helm chart called `v1k0d3n/kubevirt-redfish`. I ***am not** going to recommend downloading a custom `values.yaml` in this case, but instead I recommend you edit/use the [sample values file](./examples/custom-values-kubevirt-redfish.yaml).

   ```bash
   curl -L -o custom-values-kubevirt-redfish.yaml https://raw.githubusercontent.com/kubevirt/redfish-controller/refs/heads/main/docs/examples/custom-values-kubevirt-redfish.yaml
   ```

   **EXTREMELY IMPORTANT:** *This is a demonstration, so assuming that you're keeping the same namespace names (and all the other settings), than the only field you should have to edit is the `route.host` field (which is configured to be: `kubevirt-redfish-jinkit-kvm.apps.hub.lab.ocp.run`). You can change the `<clustername>.<domainname>` but if you've left all the other settings (as I recommended), then you can leave the other parts of the URL (i.e. `kubevirt-redfish-jinkit-kvm.apps.`).

2. Once you have *minimally* configure/edit the `custom-values-kubevirt-redfish.yaml` file for your environment, you can install the chart by using the following command.

   ```bash
   helm upgrade --install kubevirt-redfish v1k0d3n/kubevirt-redfish --version 0.2.3 -f custom-values-kubevirt-redfish.yaml -n jinkit-kvm
   ```

## Step 3: Deploy OpenShift via ClusterInstance

1. Download your OpenShift `pull-secret.txt` from the following link: [HERE](https://console.redhat.com/openshift/downloads#tool-pull-secret).

2. Now create you can create a secret, using this file, with the following command below. This command assumes that the `pull-secret.txt` is in your current directory.

   ```bash
   oc create secret generic pullsecret-jinkit-kvm \
     --from-file=.dockerconfigjson=pull-secret.txt \
     --type=kubernetes.io/dockerconfigjson \
     -n jinkit-kvm
   ```

3. Now apply the example `BareMetalHost` Redfish secrets (username/password) from the [examples folder](./exmaples/).

   ```bash
   curl -L -o bmc-ztp-jinkit-kvm.yaml https://raw.githubusercontent.com/kubevirt/redfish-controller/refs/heads/main/docs/examples/bmc-ztp-jinkit-kvm.yaml
   
   oc apply -f bmc-ztp-jinkit-kvm.yaml
   ```

4. Apply the **4.20** `ClusterImageSet` from the [examples folder](./exmaples/).

   ```bash
   curl -L -o cis-prerelease-420.yaml https://raw.githubusercontent.com/kubevirt/redfish-controller/refs/heads/main/docs/examples/cis-prerelease-420.yaml
   
   oc apply -f cis-prerelease-420.yaml
   ```

5. **EXTREMELY IMPORTANT:** For this next step, you will have roughly 26 edits that need to be made. It's the `ClusterInstance` deployment, which is our final manifest. Let's download it first, and then I will tell you what fields need changed.

   ```bash
   curl -L -o ci-ztp-jinkit-kvm.yaml https://raw.githubusercontent.com/kubevirt/redfish-controller/refs/heads/main/docs/examples/ci-ztp-jinkit-kvm.yaml
   ```

   After downloading the `ci-ztp-jinkit-kvm.yaml` manifest, you will need to search for the string `changeme` scattered throughout the YAML file. There are 26 occurances, but they are all fairly standard fields. Please review my full example called [`ci-ztp-jinkit-kvm.yaml`](./examples/ci-ztp-jinkit-kvm-example.yaml) if you need some helpful hints.

6. Once you have made the approprate edits to the `ci-ztp-jinkit-kvm.yaml` manifest, apply it.

   ```bash
   oc apply -f ci-ztp-jinkit-kvm.yaml
   ```

## Validation and Testing

While your deployment is starting to deploy in the background, you can explore the Redfish endpoint. Let's cover some examples that you should see in your environment.

1. **Review All Managed Systems**

   **Client Request:**
   ```bash
   curl -sk -u admin:admin123 https://kubevirt-redfish-jinkit-kvm.apps.hub.lab.ocp.run/redfish/v1/Systems | jq
   ```

   **Server Response:**
   ```json
   {
     "@odata.context": "/redfish/v1/$metadata#ComputerSystemCollection.ComputerSystemCollection",
     "@odata.id": "/redfish/v1/Systems",
     "@odata.type": "#ComputerSystemCollection.ComputerSystemCollection",
     "Name": "Computer System Collection",
     "Members": [
       {
         "@odata.id": "/redfish/v1/Systems/ztp-jinkit-kvm-00"
       },
       {
         "@odata.id": "/redfish/v1/Systems/ztp-jinkit-kvm-01"
       },
       {
         "@odata.id": "/redfish/v1/Systems/ztp-jinkit-kvm-02"
       }
     ],
     "Members@odata.count": 3
   }
   ```

2. **Review a Single Managed System**

   **Client Request:**
   ```bash
   curl -sk -u admin:admin123 https://kubevirt-redfish-jinkit-kvm.apps.hub.lab.ocp.run/redfish/v1/Systems/ztp-jinkit-kvm-00 | jq
   ```

   **Server Response:**
   ```json
   {
     "@odata.context": "/redfish/v1/$metadata#ComputerSystem.ComputerSystem",
     "@odata.id": "/redfish/v1/Systems/ztp-jinkit-kvm-00",
     "@odata.type": "#ComputerSystem.v1_15_0.ComputerSystem",
     "@odata.etag": "W/\"1756160934\"",
     "Id": "ztp-jinkit-kvm-00",
     "Name": "ztp-jinkit-kvm-00",
     "SystemType": "Virtual",
     "Status": {
       "State": "Enabled",
       "Health": "OK"
     },
     "PowerState": "On",
     "MemorySummary": {
       "TotalSystemMemoryGiB": 48
     },
     "Memory": {
       "@odata.id": "/redfish/v1/Systems/ztp-jinkit-kvm-00/Memory"
     },
     "Processors": {
       "@odata.id": "/redfish/v1/Systems/ztp-jinkit-kvm-00/Processors"
     },
     "ProcessorSummary": {
       "Count": 1,
       "CoreCount": 20,
       "LogicalProcessorCount": 20,
       "ThreadingEnabled": false
     },
     "Storage": {
       "@odata.id": "/redfish/v1/Systems/ztp-jinkit-kvm-00/Storage"
     },
     "EthernetInterfaces": {
       "@odata.id": "/redfish/v1/Systems/ztp-jinkit-kvm-00/EthernetInterfaces"
     },
     "VirtualMedia": {
       "@odata.id": "/redfish/v1/Systems/ztp-jinkit-kvm-00/VirtualMedia"
     },
     "Boot": {
       "BootSourceOverrideEnabled": "Continuous",
       "BootSourceOverrideTarget": "Hdd",
       "BootSourceOverrideTarget@Redfish.AllowableValues": [
         "Cd",
         "Hdd"
       ],
       "BootSourceOverrideMode": "UEFI",
       "UefiTargetBootSourceOverride": "/0x31/0x33/0x01/0x01"
     },
     "Actions": {
       "#ComputerSystem.Reset": {
         "target": "/redfish/v1/Systems/ztp-jinkit-kvm-00/Actions/ComputerSystem.Reset",
         "ResetType@Redfish.AllowableValues": [
           "On",
           "ForceOff",
           "GracefulShutdown",
           "ForceRestart",
           "GracefulRestart",
           "Pause",
           "Resume"
         ]
       }
     },
     "Links": {
       "ManagedBy": [
         {
           "@odata.id": "/redfish/v1/Managers/1"
         }
       ]
     },
     "Oem": {
       "KubeVirt": {
         "Processors": {
           "Sockets": 1,
           "CoresPerSocket": 20,
           "ThreadsPerCore": 1
         }
       }
     }
   }
   ```

3. **Review the Hardware of a Managed System**

   A system also exposes its processors, memory, disks and network interfaces. The examples below come from a virtual machine named `my-vm` with 2 sockets, 2 cores per socket, 2 threads per core, 512 MiB of memory, a root disk from a container image and a 3 GiB SATA disk. All of them are read-only and follow the links in the system response.

   **Processors** (one entry per socket, `/Processors/CPU0`, `/Processors/CPU1`):

   ```bash
   curl -sk -u admin:admin123 https://<redfish-route>/redfish/v1/Systems/my-vm/Processors/CPU0 | jq
   ```

   ```json
   {
     "@odata.context": "/redfish/v1/$metadata#Processor.Processor",
     "@odata.id": "/redfish/v1/Systems/my-vm/Processors/CPU0",
     "@odata.type": "#Processor.v1_0_0.Processor",
     "Id": "CPU0",
     "Name": "vCPU Socket 0",
     "Socket": "0",
     "ProcessorType": "CPU",
     "ProcessorArchitecture": "x86",
     "InstructionSet": "x86-64",
     "TotalCores": 2,
     "TotalThreads": 4,
     "Model": "host-model",
     "Status": {
       "State": "Enabled",
       "Health": "OK"
     }
   }
   ```

   **Memory** (a single module, `/Memory/1`). `CapacityMiB` is the standard field and `Oem.KubeVirt.CapacityGiB` repeats it in GiB:

   ```json
   {
     "@odata.context": "/redfish/v1/$metadata#Memory.Memory",
     "@odata.id": "/redfish/v1/Systems/my-vm/Memory/1",
     "@odata.type": "#Memory.v1_1_0.Memory",
     "Id": "1",
     "Name": "System Memory",
     "MemoryType": "DRAM",
     "CapacityMiB": 512,
     "Status": {
       "State": "Enabled",
       "Health": "OK"
     },
     "Oem": {
       "KubeVirt": {
         "CapacityGiB": 0.5
       }
     }
   }
   ```

   **Disks** (`/Storage/1` lists every disk of the VM, CD-ROMs excluded because they are VirtualMedia; each drive is under `/Storage/1/Drives/{name}`). A disk whose size is not declared in the VM, such as a container image or a cloud-init disk, has no `CapacityBytes`:

   ```json
   {
     "@odata.context": "/redfish/v1/$metadata#Drive.Drive",
     "@odata.id": "/redfish/v1/Systems/my-vm/Storage/1/Drives/scratch",
     "@odata.type": "#Drive.v1_0_0.Drive",
     "Id": "scratch",
     "Name": "scratch",
     "Protocol": "SATA",
     "CapacityBytes": 3221225472,
     "Status": {
       "State": "Enabled",
       "Health": "OK"
     },
     "Oem": {
       "KubeVirt": {
         "CapacityGiB": 3
       }
     }
   }
   ```

   **Network interfaces** (`/EthernetInterfaces` lists every NIC). `LinkStatus` is `LinkUp` while the VM is running and `NoLink` otherwise. The MAC address comes from the running VM, or from the VM definition when it is stopped, and is omitted if neither has one. Virtual NICs have no link speed, so `SpeedMbps` is not reported:

   ```json
   {
     "@odata.context": "/redfish/v1/$metadata#EthernetInterface.EthernetInterface",
     "@odata.id": "/redfish/v1/Systems/my-vm/EthernetInterfaces/default",
     "@odata.type": "#EthernetInterface.v1_1_0.EthernetInterface",
     "Id": "default",
     "Name": "default",
     "MACAddress": "02:11:22:33:44:55",
     "LinkStatus": "LinkUp",
     "IPv4Addresses": [
       {
         "Address": "10.128.2.164"
       }
     ],
     "Status": {
       "State": "Enabled",
       "Health": "OK"
     }
   }
   ```

   `ProcessorSummary` follows the Redfish definitions: `Count` is the number of sockets, `CoreCount` is sockets x cores and `LogicalProcessorCount` is sockets x cores x threads. The processor `Model` is the CPU model configured on the VM (for example `host-model`) and is omitted when the VM does not set one.

## Environment Cleanup

To clean up your environment, you can run the following series of commands.

1. Stop each of the VMs manually. Since you have Redfish deployed, you can leverage the Redfish endpoint for this.

   ```bash
   BASE="https://kubevirt-redfish-jinkit-kvm.apps.hub.lab.ocp.run"
   AUTH="admin:admin123"

   for H in ztp-jinkit-kvm-00 ztp-jinkit-kvm-01 ztp-jinkit-kvm-02; do
     echo "$H -> ForceOff requested"
     curl -sk -u "$AUTH" \
       -H 'Content-Type: application/json' \
       -X POST \
       -d '{"ResetType":"ForceOff"}' \
       "$BASE/redfish/v1/Systems/$H/Actions/ComputerSystem.Reset" >/dev/null

     while :; do
       PS=$(curl -sk -u "$AUTH" "$BASE/redfish/v1/Systems/$H" | jq -r '.PowerState')
       echo "  PowerState: $PS"
       [ "$PS" = "Off" ] && break
       sleep 2
     done
   done
   ```

2. Next delete the `ClusterInstance` object, using the `ci-ztp-jinkit-kvm.yaml` manifest.

   ```bash
   oc delete -f ci-ztp-jinkit-kvm.yaml
   ```

   **NOTE:** This may take a couple of minutes to clean up. If this doesn't stop after 2-3 minutes, you can forcibly stop the process (CTL+C), and continue with the next steps.

3. Once that is complete, you can safely delete the virtual machines we created for the project (using Helm).

   ```bash
   helm delete vms-jinkit-kvm -n jinkit-kvm
   ```

4. Next you can delete the `kubevirt-redfish` Helm chart.

   ```bash
   helm delete kubevirt-redfish -n jinkit-kvm
   ```

5. Be sure to remove our namespace finalizer that we applied to `jinkit-kvm` (which prevents accidental namespace deletion).

   ```bash
   oc patch namespace jinkit-kvm --type='merge' -p='{"metadata":{"finalizers":[]}}'
   ```

6. Finally we can safely delete our demonstration `Namespace`.

   ```bash
   oc delete project jinkit-kvm
   ```

   **NOTE:** It's possible, that if you did not let the `ClusterInstance` process complete (which **expectedly** will take some time), then the previous command forced the namespace deletion because the ClusterInstance was still attempting to clean up resources - including the namespace.