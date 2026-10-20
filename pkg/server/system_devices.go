/*
 * This file is part of the KubeVirt Redfish project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2026 KubeVirt Redfish project and its authors.
 *
 */

package server

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/kubevirt/redfish-controller/pkg/config"
	"github.com/kubevirt/redfish-controller/pkg/kubevirt"
	"github.com/kubevirt/redfish-controller/pkg/logger"
	"github.com/kubevirt/redfish-controller/pkg/redfish"
)

// memoryID and storageID are the fixed member IDs: a VM exposes one logical
// memory module and one storage controller that holds all of its disks.
const (
	memoryID  = "1"
	storageID = "1"
)

var okStatus = redfish.Status{State: "Enabled", Health: "OK"}

// handleSystemDevices serves the read-only hardware sub-resources of a system:
// Memory, Processors, Storage (with Drives) and EthernetInterfaces. The caller has
// already validated the method and resolved the VM with its access check. pathParts is the split
// request path. For "/redfish/v1/Systems/vm-1/Storage/1/Drives/root" that is
// ["", "redfish", "v1", "Systems", "vm-1", "Storage", "1", "Drives", "root"], so
// pathParts[5:] starts at the sub-resource name ("Storage").
func (s *Server) handleSystemDevices(w http.ResponseWriter, r *http.Request, namespace, vmName string, pathParts []string) {
	// Drop a trailing empty element: "/redfish/v1/Systems/vm-1/Storage/" splits
	// into [... "Storage", ""], which must be handled like "/Storage".
	rest := pathParts[5:]
	if rest[len(rest)-1] == "" {
		rest = rest[:len(rest)-1]
	}
	base := "/redfish/v1/Systems/" + config.GenerateSystemID(s.currentConfig().SystemIDConvention, namespace, vmName)

	switch rest[0] {
	case "Memory":
		s.serveMemory(w, namespace, vmName, base, rest)
	case "Processors":
		s.serveProcessors(w, namespace, vmName, base, rest)
	case "Storage":
		s.serveStorage(w, namespace, vmName, base, rest)
	case "EthernetInterfaces":
		s.serveEthernetInterfaces(w, namespace, vmName, base, rest)
	}
}

func (s *Server) serveMemory(w http.ResponseWriter, namespace, vmName, base string, rest []string) {
	self := base + "/Memory"
	switch {
	case len(rest) == 1:
		s.writeCollection(w, self, "#MemoryCollection.MemoryCollection", "Memory Collection", []string{self + "/" + memoryID})
	case len(rest) == 2 && rest[1] == memoryID:
		mib, err := s.kubevirtClient.VMMemoryMiB(namespace, vmName)
		if err != nil {
			logger.Error("Failed to get memory for VM %s/%s: %v", namespace, vmName, err)
			s.sendInternalError(w, "Failed to get memory information")
			return
		}
		mem := redfish.Memory{
			OdataContext: "/redfish/v1/$metadata#Memory.Memory",
			OdataID:      self + "/" + memoryID,
			OdataType:    "#Memory.v1_1_0.Memory",
			ID:           memoryID,
			Name:         "System Memory",
			MemoryType:   "DRAM",
			CapacityMiB:  mib,
			Status:       okStatus,
		}
		if mib > 0 {
			mem.Oem = oemCapacity(float64(mib) / 1024)
		}
		s.writeResource(w, mem)
	default:
		s.sendNotFound(w, "Memory resource not found")
	}
}

func (s *Server) serveProcessors(w http.ResponseWriter, namespace, vmName, base string, rest []string) {
	self := base + "/Processors"
	cpu, err := s.kubevirtClient.GetVMCPUTopology(namespace, vmName)
	if err != nil {
		logger.Error("Failed to get CPU topology for VM %s/%s: %v", namespace, vmName, err)
		s.sendInternalError(w, "Failed to get processor information")
		return
	}

	if len(rest) == 1 {
		members := make([]string, 0, cpu.Sockets)
		for i := 0; i < cpu.Sockets; i++ {
			members = append(members, fmt.Sprintf("%s/CPU%d", self, i))
		}
		s.writeCollection(w, self, "#ProcessorCollection.ProcessorCollection", "Processor Collection", members)
		return
	}
	socket, err := strconv.Atoi(strings.TrimPrefix(rest[1], "CPU"))
	if len(rest) != 2 || !strings.HasPrefix(rest[1], "CPU") || err != nil || socket < 0 || socket >= cpu.Sockets || rest[1] != fmt.Sprintf("CPU%d", socket) {
		s.sendNotFound(w, "Processor not found")
		return
	}
	arch, isa := redfishArchitecture(cpu.Architecture)
	s.writeResource(w, redfish.Processor{
		OdataContext:          "/redfish/v1/$metadata#Processor.Processor",
		OdataID:               fmt.Sprintf("%s/CPU%d", self, socket),
		OdataType:             "#Processor.v1_0_0.Processor",
		ID:                    rest[1],
		Name:                  fmt.Sprintf("vCPU Socket %d", socket),
		Socket:                fmt.Sprint(socket),
		ProcessorType:         "CPU",
		ProcessorArchitecture: arch,
		InstructionSet:        isa,
		TotalCores:            cpu.Cores,
		TotalThreads:          cpu.Cores * cpu.Threads,
		Model:                 cpu.Model,
		Status:                okStatus,
	})
}

func (s *Server) serveStorage(w http.ResponseWriter, namespace, vmName, base string, rest []string) {
	self := base + "/Storage"
	if len(rest) == 1 {
		s.writeCollection(w, self, "#StorageCollection.StorageCollection", "Storage Collection", []string{self + "/" + storageID})
		return
	}
	if rest[1] != storageID {
		s.sendNotFound(w, "Storage resource not found")
		return
	}

	member := self + "/" + storageID

	switch {
	case len(rest) == 2:
		disks, err := s.kubevirtClient.GetVMDisks(namespace, vmName)
		if err != nil {
			logger.Error("Failed to get disks for VM %s/%s: %v", namespace, vmName, err)
			s.sendInternalError(w, "Failed to get storage information")
			return
		}
		storage := redfish.Storage{
			OdataContext: "/redfish/v1/$metadata#Storage.Storage",
			OdataID:      member,
			OdataType:    "#Storage.v1_0_0.Storage",
			ID:           storageID,
			Name:         "VM Disks",
			Status:       okStatus,
			Drives:       []redfish.Link{},
			DrivesCount:  len(disks),
		}
		for _, d := range disks {
			storage.Drives = append(storage.Drives, redfish.Link{OdataID: member + "/Drives/" + d.Name})
		}
		s.writeResource(w, storage)
	case len(rest) == 4 && rest[2] == "Drives":
		d, err := s.kubevirtClient.GetVMDisk(namespace, vmName, rest[3])
		if err != nil {
			logger.Error("Failed to get disk %s of VM %s/%s: %v", rest[3], namespace, vmName, err)
			s.sendInternalError(w, "Failed to get storage information")
			return
		}
		if d == nil {
			s.sendNotFound(w, "Drive not found")
			return
		}
		drive := redfish.Drive{
			OdataContext:  "/redfish/v1/$metadata#Drive.Drive",
			OdataID:       member + "/Drives/" + d.Name,
			OdataType:     "#Drive.v1_0_0.Drive",
			ID:            d.Name,
			Name:          d.Name,
			CapacityBytes: d.CapacityBytes,
			Status:        okStatus,
		}
		if d.CapacityBytes != nil {
			drive.Oem = oemCapacity(float64(*d.CapacityBytes) / (1 << 30))
		}
		if d.Bus == "sata" {
			drive.Protocol = "SATA"
		}
		s.writeResource(w, drive)
	default:
		s.sendNotFound(w, "Storage resource not found")
	}
}

func (s *Server) serveEthernetInterfaces(w http.ResponseWriter, namespace, vmName, base string, rest []string) {
	self := base + "/EthernetInterfaces"
	nics, err := s.kubevirtClient.GetVMNICs(namespace, vmName)
	if err != nil {
		logger.Error("Failed to get network interfaces for VM %s/%s: %v", namespace, vmName, err)
		s.sendInternalError(w, "Failed to get network interface information")
		return
	}

	if len(rest) == 1 {
		members := make([]string, 0, len(nics))
		for _, n := range nics {
			members = append(members, self+"/"+n.Name)
		}
		s.writeCollection(w, self, "#EthernetInterfaceCollection.EthernetInterfaceCollection", "Ethernet Interface Collection", members)
		return
	}
	if len(rest) != 2 {
		s.sendNotFound(w, "Ethernet interface not found")
		return
	}
	for _, n := range nics {
		if n.Name != rest[1] {
			continue
		}
		eth := redfish.EthernetInterface{
			OdataContext: "/redfish/v1/$metadata#EthernetInterface.EthernetInterface",
			OdataID:      self + "/" + n.Name,
			OdataType:    "#EthernetInterface.v1_1_0.EthernetInterface",
			ID:           n.Name,
			Name:         n.Name,
			MACAddress:   n.MAC,
			LinkStatus:   "NoLink",
			Status:       okStatus,
		}
		// A reported "down" is a disconnected virtual NIC on a running VM; an empty
		// state (not reported) on a running NIC is treated as up.
		if n.Running && n.LinkState != "down" {
			eth.LinkStatus = "LinkUp"
		}
		for _, ip := range n.IPs {
			switch p := net.ParseIP(ip); {
			case p == nil:
			case p.To4() != nil:
				eth.IPv4 = append(eth.IPv4, redfish.IPv4Address{Address: ip})
			default:
				eth.IPv6 = append(eth.IPv6, redfish.IPv6Address{Address: ip})
			}
		}
		s.writeResource(w, eth)
		return
	}
	s.sendNotFound(w, "Ethernet interface not found")
}

// redfishArchitecture maps a KubeVirt architecture to the Redfish
// ProcessorArchitecture and InstructionSet values. Unknown architectures map to "".
func redfishArchitecture(kubevirtArch string) (arch, instructionSet string) {
	switch kubevirtArch {
	case "amd64":
		return "x86", "x86-64"
	case "arm64":
		return "ARM", "ARM-A64"
	}
	return "", ""
}

// processorFields reads the vCPU topology of a VM and returns the ProcessorSummary
// and Oem for the ComputerSystem. If the topology cannot be read both are nil, so
// they are left out of the response instead of showing a guessed topology.
func (s *Server) processorFields(namespace, vmName string) (*redfish.ProcessorSummary, *redfish.SystemOem) {
	cpu, err := s.kubevirtClient.GetVMCPUTopology(namespace, vmName)
	if err != nil {
		logger.Error("Failed to get CPU topology for VM %s/%s, leaving it out of the response: %v", namespace, vmName, err)
		return nil, nil
	}
	summary := processorSummary(cpu)
	return &summary, systemOem(cpu)
}

// processorSummary maps a VM's vCPU topology to the Redfish ProcessorSummary.
func processorSummary(cpu kubevirt.CPUTopology) redfish.ProcessorSummary {
	return redfish.ProcessorSummary{
		Count:                 cpu.Sockets,
		CoreCount:             cpu.Sockets * cpu.Cores,
		LogicalProcessorCount: cpu.VCPUs(),
		ThreadingEnabled:      cpu.Threads > 1,
		Model:                 cpu.Model,
	}
}

// systemOem spells out the vCPU layout that the standard counts are derived
// from. ProcessorSummary has no Oem property in any schema version, so it
// lives in the system-level Oem.
func systemOem(cpu kubevirt.CPUTopology) *redfish.SystemOem {
	oem := &redfish.SystemOem{}
	oem.KubeVirt.Processors.Sockets = cpu.Sockets
	oem.KubeVirt.Processors.CoresPerSocket = cpu.Cores
	oem.KubeVirt.Processors.ThreadsPerCore = cpu.Threads
	return oem
}

func oemCapacity(gib float64) *redfish.OemCapacity {
	o := &redfish.OemCapacity{}
	o.KubeVirt.CapacityGiB = gib
	return o
}

func (s *Server) writeCollection(w http.ResponseWriter, id, odataType, name string, members []string) {
	links := make([]redfish.Link, 0, len(members))
	for _, m := range members {
		links = append(links, redfish.Link{OdataID: m})
	}
	s.writeResource(w, redfish.ResourceCollection{
		OdataContext: fmt.Sprintf("/redfish/v1/$metadata#%s", odataType[1:]),
		OdataID:      id,
		OdataType:    odataType,
		Name:         name,
		Members:      links,
		MembersCount: len(links),
	})
}

func (s *Server) writeResource(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	s.setCacheHeaders(w, "resource")
	s.encodeJSONResponse(w, v)
}
