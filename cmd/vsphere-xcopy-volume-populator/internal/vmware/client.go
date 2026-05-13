package vmware

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"strings"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/cli/esx"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

	"k8s.io/klog/v2"
)

//go:generate go run go.uber.org/mock/mockgen -destination=mocks/vmware_mock_client.go -package=vmware_mocks . Client
type Client interface {
	GetEsxByVm(ctx context.Context, vmName string) (*object.HostSystem, error)
	RunEsxCommand(ctx context.Context, host *object.HostSystem, command []string) ([]esx.Values, error)
	GetDatastore(ctx context.Context, dc *object.Datacenter, datastore string) (*object.Datastore, error)
	// GetVMDiskBacking returns disk backing information for detecting disk type (VVol, RDM, VMDK).
	// When warmOffload is true, snapshot-aware detection is used: the Parent chain is walked to
	// match the path and to find the base disk's BackingObjectId, identifying vVol-backed disks
	// even when a precopy snapshot is active and the top-level backing filename has changed.
	GetVMDiskBacking(ctx context.Context, vmId string, vmdkPath string, warmOffload bool) (*DiskBacking, error)
}

// DiskBacking contains information about the disk backing type
type DiskBacking struct {
	// VVolId is set if the disk is VVol-backed
	VVolId string
	// IsRDM is true if the disk is a Raw Device Mapping
	IsRDM bool
	// DeviceName is the underlying device name
	DeviceName string
}

type VSphereClient struct {
	*govmomi.Client
}

func NewClient(vcenterUrl, username, password string) (Client, error) {
	ctx := context.Background()
	u, err := soap.ParseURL(vcenterUrl)
	if err != nil {
		return nil, fmt.Errorf("Failed parsing vCenter URL: %w", err)
	}
	u.User = url.UserPassword(username, password)

	c, err := govmomi.NewClient(ctx, u, true)
	if err != nil {
		return nil, fmt.Errorf("Failed creating vSphere client: %w", err)
	}

	return &VSphereClient{Client: c}, nil
}

func (c *VSphereClient) RunEsxCommand(ctx context.Context, host *object.HostSystem, command []string) ([]esx.Values, error) {
	executor, err := esx.NewExecutor(ctx, c.Client.Client, host.Reference())
	if err != nil {
		return nil, err
	}

	// Invoke esxcli command
	klog.Infof("about to run esxcli command %s", command)
	res, err := executor.Run(ctx, command)
	if err != nil {
		klog.Errorf("Failed to run esxcli command %v: %s", command, err)
		if fault, ok := err.(*esx.Fault); ok {
			if parsedFault, parseErr := ErrToFault(fault); parseErr == nil {
				klog.Errorf("ESX CLI Fault - Type: %s, Messages: %v", parsedFault.Type, parsedFault.ErrMsgs)
			} else {
				klog.Errorf("Failed to parse fault details: %v", parseErr)
			}
		}
		return nil, err
	}
	for _, valueMap := range res.Values {
		message, _ := valueMap["message"]
		status, statusExists := valueMap["status"]
		klog.Infof("esxcli result %v, message %s, status %v", valueMap, message, status)
		if statusExists && strings.Join(status, "") != "0" {
			return nil, fmt.Errorf("Failed to invoke vmkfstools: %v", message)
		}
	}
	return res.Values, nil
}

func (c *VSphereClient) GetEsxByVm(ctx context.Context, vmId string) (*object.HostSystem, error) {
	finder := find.NewFinder(c.Client.Client, true)
	datacenters, err := finder.DatacenterList(ctx, "*")
	if err != nil {
		return nil, fmt.Errorf("failed getting datacenters: %w", err)
	}

	var vm *object.VirtualMachine
	for _, dc := range datacenters {
		finder.SetDatacenter(dc)
		result, err := finder.VirtualMachine(ctx, vmId)
		if err != nil {
			if _, ok := err.(*find.NotFoundError); !ok {
				return nil, fmt.Errorf("error searching for VM in Datacenter '%s': %w", dc.Name(), err)
			}
		} else {
			vm = result
			fmt.Printf("found vm %v\n", vm)
			break
		}
	}
	if vm == nil {
		moref := types.ManagedObjectReference{Type: "VirtualMachine", Value: vmId}
		vm = object.NewVirtualMachine(c.Client.Client, moref)
	}
	if vm == nil {
		return nil, fmt.Errorf("failed to find VM with ID %s", vmId)
	}

	var vmProps mo.VirtualMachine
	err = vm.Properties(ctx, vm.Reference(), []string{"runtime.host"}, &vmProps)
	if err != nil {
		return nil, fmt.Errorf("failed to get VM properties: %w", err)
	}

	hostRef := vmProps.Runtime.Host
	host := object.NewHostSystem(c.Client.Client, *hostRef)
	if host == nil {
		return nil, fmt.Errorf("failed to find host: %w", err)
	}
	return host, nil
}

func (c *VSphereClient) GetDatastore(ctx context.Context, dc *object.Datacenter, datastore string) (*object.Datastore, error) {
	finder := find.NewFinder(c.Client.Client, false)
	finder.SetDatacenter(dc)

	ds, err := finder.Datastore(ctx, datastore)
	if err != nil {
		return nil, fmt.Errorf("Failed to find datastore %s: %w", datastore, err)
	}

	return ds, nil
}

// GetVMDiskBacking retrieves disk backing information to determine disk type
func (c *VSphereClient) GetVMDiskBacking(ctx context.Context, vmId string, vmdkPath string, warmOffload bool) (*DiskBacking, error) {
	finder := find.NewFinder(c.Client.Client, true)
	datacenters, err := finder.DatacenterList(ctx, "*")
	if err != nil {
		return nil, fmt.Errorf("failed getting datacenters: %w", err)
	}

	var vm *object.VirtualMachine
	for _, dc := range datacenters {
		finder.SetDatacenter(dc)
		result, err := finder.VirtualMachine(ctx, vmId)
		if err != nil {
			if _, ok := err.(*find.NotFoundError); !ok {
				return nil, fmt.Errorf("error searching for VM in Datacenter '%s': %w", dc.Name(), err)
			}
		} else {
			vm = result
			break
		}
	}
	if vm == nil {
		moref := types.ManagedObjectReference{Type: "VirtualMachine", Value: vmId}
		vm = object.NewVirtualMachine(c.Client.Client, moref)
	}
	if vm == nil {
		return nil, fmt.Errorf("failed to find VM with ID %s", vmId)
	}

	// Get VM configuration to inspect disk devices
	var vmProps mo.VirtualMachine
	err = vm.Properties(ctx, vm.Reference(), []string{"config.hardware.device"}, &vmProps)
	if err != nil {
		return nil, fmt.Errorf("failed to get VM properties: %w", err)
	}

	// Normalize vmdkPath for comparison (remove brackets and spaces)
	normalizedPath := strings.ToLower(vmdkPath)

	// Find the disk matching the vmdkPath
	for _, device := range vmProps.Config.Hardware.Device {
		disk, ok := device.(*types.VirtualDisk)
		if !ok {
			continue
		}

		// Check different backing types
		switch backing := disk.Backing.(type) {
		case *types.VirtualDiskFlatVer2BackingInfo:
			if warmOffload {
				if !flatBackingMatchesPath(backing, normalizedPath, vmdkPath) {
					continue
				}
				vvolId := findVVolBackingObjectId(backing)
				if vvolId != "" {
					klog.V(2).Infof("disk is VVol-backed: vmdk=%s backing_object_id=%s", vmdkPath, vvolId)
					return &DiskBacking{
						VVolId:     vvolId,
						IsRDM:      false,
						DeviceName: backing.FileName,
					}, nil
				}
				klog.V(2).Infof("disk is VMDK-backed: vmdk=%s", vmdkPath)
				return &DiskBacking{
					VVolId:     "",
					IsRDM:      false,
					DeviceName: backing.FileName,
				}, nil
			}

			// Original path for non-warm-offload vendors
			if !strings.Contains(strings.ToLower(backing.FileName), normalizedPath) &&
				!strings.Contains(normalizedPath, strings.ToLower(backing.FileName)) {
				klog.Infof("backing.FileName: %s, normalizedPath: %s", backing.FileName, normalizedPath)
				if !diskPathMatches(backing.FileName, vmdkPath) {
					klog.Infof("vmdkpath does not match: %s, %s", backing.FileName, vmdkPath)
					continue
				}
			}
			if backing.BackingObjectId != "" {
				klog.Infof("Disk %s is VVol-backed (BackingObjectId: %s)", vmdkPath, backing.BackingObjectId)
				return &DiskBacking{
					VVolId:     backing.BackingObjectId,
					IsRDM:      false,
					DeviceName: backing.FileName,
				}, nil
			}
			klog.Infof("Disk %s is VMDK-backed", vmdkPath)
			return &DiskBacking{
				VVolId:     "",
				IsRDM:      false,
				DeviceName: backing.FileName,
			}, nil

		case *types.VirtualDiskRawDiskMappingVer1BackingInfo:
			// Check if this disk matches
			if !strings.Contains(strings.ToLower(backing.FileName), normalizedPath) &&
				!strings.Contains(normalizedPath, strings.ToLower(backing.FileName)) {
				if !diskPathMatches(backing.FileName, vmdkPath) {
					continue
				}
			}

			klog.Infof("Disk %s is RDM-backed (DeviceName: %s)", vmdkPath, backing.DeviceName)
			return &DiskBacking{
				VVolId:     "",
				IsRDM:      true,
				DeviceName: backing.DeviceName,
			}, nil
		}
	}

	// If we couldn't find the disk, return default VMDK type
	klog.Infof("Could not find specific disk %s, assuming VMDK type", vmdkPath)
	return &DiskBacking{
		VVolId:     "",
		IsRDM:      false,
		DeviceName: "",
	}, nil
}

// flatBackingMatchesPath reports whether vmdkPath matches the supplied flat
// backing or any backing in its Parent chain. Walking the chain is required
// when a snapshot is active because the snapshot backing at the top of the
// chain has a different filename than the originally-requested base path;
// for non-snapshotted disks Parent is nil and the loop runs once.
func flatBackingMatchesPath(b *types.VirtualDiskFlatVer2BackingInfo, normalizedPath, vmdkPath string) bool {
	for cur := b; cur != nil; cur = cur.Parent {
		fn := strings.ToLower(cur.FileName)
		if strings.Contains(fn, normalizedPath) ||
			strings.Contains(normalizedPath, fn) ||
			diskPathMatches(cur.FileName, vmdkPath) {
			return true
		}
	}
	return false
}

// findVVolBackingObjectId returns the first non-empty BackingObjectId found
// while walking the supplied flat backing's Parent chain, or "" if none is
// present. Walking is required because snapshot backings carry an empty
// BackingObjectId even when the underlying base disk is vVol-backed.
func findVVolBackingObjectId(b *types.VirtualDiskFlatVer2BackingInfo) string {
	for cur := b; cur != nil; cur = cur.Parent {
		if cur.BackingObjectId != "" {
			return cur.BackingObjectId
		}
	}
	return ""
}

// diskPathMatches compares two VMDK paths accounting for different formats
func diskPathMatches(path1, path2 string) bool {
	// Extract datastore and filename from both paths
	// Format: "[datastore] folder/file.vmdk"
	normalize := func(p string) string {
		p = strings.TrimSpace(p)
		p = strings.ToLower(p)
		// Remove brackets from datastore
		p = strings.ReplaceAll(p, "[", "")
		p = strings.ReplaceAll(p, "]", "")
		return p
	}

	return normalize(path1) == normalize(path2)
}

type Obj struct {
	XMLName          xml.Name `xml:"urn:vim25 obj"`
	VersionID        string   `xml:"versionId,attr"`
	Type             string   `xml:"http://www.w3.org/2001/XMLSchema-instance type,attr"`
	Fault            Fault    `xml:"fault"`
	LocalizedMessage string   `xml:"localizedMessage"`
}

type Fault struct {
	Type    string   `xml:"http://www.w3.org/2001/XMLSchema-instance type,attr"`
	ErrMsgs []string `xml:"errMsg"`
}

func ErrToFault(err error) (*Fault, error) {
	f, ok := err.(*esx.Fault)
	if ok {
		var obj Obj
		decoder := xml.NewDecoder(strings.NewReader(f.Detail))
		err := decoder.Decode(&obj)
		if err != nil {
			return nil, fmt.Errorf("failed to decode from xml to fault: %w", err)
		}
		return &obj.Fault, nil
	}
	return nil, fmt.Errorf("error is not of type esx.Fault")
}
