//  Copyright 2017 Google Inc. All Rights Reserved.
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package daisy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sync"
	"time"

	daisyCompute "github.com/GoogleCloudPlatform/compute-image-tools/daisy/compute"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
)

// CreateInstances is a Daisy CreateInstances workflow step.
type CreateInstances []*CreateInstance

// CreateInstance creates a GCE instance. Output of serial port 1 will be
// streamed to the daisy logs directory.
type CreateInstance struct {
	compute.Instance

	// Additional metadata to set for the instance.
	Metadata map[string]string `json:"metadata,omitempty"`
	// OAuth2 scopes to give the instance. If none are specified
	// https://www.googleapis.com/auth/devstorage.read_only will be added.
	Scopes []string `json:",omitempty"`
	// StartupScript is the Sources path to a startup script to use in this step.
	// This will be automatically mapped to the appropriate metadata key.
	StartupScript string `json:",omitempty"`
	// Project to create the instance in, overrides workflow Project.
	Project string `json:",omitempty"`
	// Zone to create the instance in, overrides workflow Zone.
	Zone string `json:",omitempty"`
	// Should this resource be cleaned up after the workflow?
	NoCleanup bool
	// If set Daisy will use this as the resource name instead generating a name.
	RealName string `json:",omitempty"`

	// The name of the disk as known to the Daisy user.
	daisyName string
	// Deprecated: Use RealName instead.
	ExactName bool
}

// MarshalJSON is a hacky workaround to prevent CreateInstance from using
// compute.Instance's implementation.
func (c *CreateInstance) MarshalJSON() ([]byte, error) {
	return json.Marshal(*c)
}

func logSerialOutput(ctx context.Context, w *Workflow, name string, port int64, interval time.Duration) {
	logsObj := path.Join(w.logsPath, fmt.Sprintf("%s-serial-port%d.log", name, port))
	w.logger.Printf("CreateInstances: streaming instance %q serial port %d output to gs://%s/%s", name, port, w.bucket, logsObj)
	var start int64
	var buf bytes.Buffer
	var errs int
	tick := time.Tick(interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			resp, err := w.ComputeClient.GetSerialPortOutput(w.Project, w.Zone, name, port, start)
			if err != nil {
				// Instance was deleted by this workflow.
				if _, ok := instances[w].get(name); !ok {
					return
				}
				// Instance is stopped.
				stopped, sErr := w.ComputeClient.InstanceStopped(w.Project, w.Zone, name)
				if stopped && sErr == nil {
					return
				}
				w.logger.Printf("CreateInstances: instance %q: error getting serial port: %v", name, err)
				return
			}
			start = resp.Next
			buf.WriteString(resp.Contents)
			wc := w.StorageClient.Bucket(w.bucket).Object(logsObj).NewWriter(ctx)
			wc.ContentType = "text/plain"
			if _, err := wc.Write(buf.Bytes()); err != nil {
				w.logger.Printf("CreateInstances: instance %q: error writing log to GCS: %v", name, err)
				return
			}
			if err := wc.Close(); err != nil {
				if apiErr, ok := err.(*googleapi.Error); ok && (apiErr.Code >= 500 && apiErr.Code <= 599) {
					errs++
					continue
				}
				w.logger.Printf("CreateInstances: instance %q: error saving log to GCS: %v", name, err)
				return
			}
			errs = 0
		}
	}
}

func (c *CreateInstance) populateDisks(w *Workflow) dErr {
	autonameIdx := 1
	for i, d := range c.Disks {
		d.Boot = i == 0 // TODO(crunkleton) should we do this?
		d.Mode = strOr(d.Mode, defaultDiskMode)
		p := d.InitializeParams
		if diskURLRgx.MatchString(d.Source) {
			d.Source = extendPartialURL(d.Source, c.Project)
		}
		if p != nil {
			// If name isn't set, set name to "instance-name", "instance-name-2", etc.
			if p.DiskName == "" {
				p.DiskName = c.Name
				if autonameIdx > 1 {
					p.DiskName = fmt.Sprintf("%s-%d", c.Name, autonameIdx)
				}
				autonameIdx++
			}

			// Extend SourceImage if short URL.
			if imageURLRgx.MatchString(p.SourceImage) {
				p.SourceImage = extendPartialURL(p.SourceImage, c.Project)
			}

			// Extend DiskType if short URL, or create extended URL.
			p.DiskType = strOr(p.DiskType, defaultDiskType)
			if diskTypeURLRgx.MatchString(p.DiskType) {
				p.DiskType = extendPartialURL(p.DiskType, c.Project)
			} else {
				p.DiskType = fmt.Sprintf("projects/%s/zones/%s/diskTypes/%s", c.Project, c.Zone, p.DiskType)
			}
		}
	}
	return nil
}

func (c *CreateInstance) populateMachineType() dErr {
	c.MachineType = strOr(c.MachineType, "n1-standard-1")
	if machineTypeURLRegex.MatchString(c.MachineType) {
		c.MachineType = extendPartialURL(c.MachineType, c.Project)
	} else {
		c.MachineType = fmt.Sprintf("projects/%s/zones/%s/machineTypes/%s", c.Project, c.Zone, c.MachineType)
	}
	return nil
}

func (c *CreateInstance) populateMetadata(w *Workflow) dErr {
	if c.Metadata == nil {
		c.Metadata = map[string]string{}
	}
	if c.Instance.Metadata == nil {
		c.Instance.Metadata = &compute.Metadata{}
	}
	c.Metadata["daisy-sources-path"] = "gs://" + path.Join(w.bucket, w.sourcesPath)
	c.Metadata["daisy-logs-path"] = "gs://" + path.Join(w.bucket, w.logsPath)
	c.Metadata["daisy-outs-path"] = "gs://" + path.Join(w.bucket, w.outsPath)
	if c.StartupScript != "" {
		if !w.sourceExists(c.StartupScript) {
			return errf("bad value for StartupScript, source not found: %s", c.StartupScript)
		}
		c.StartupScript = "gs://" + path.Join(w.bucket, w.sourcesPath, c.StartupScript)
		c.Metadata["startup-script-url"] = c.StartupScript
		c.Metadata["windows-startup-script-url"] = c.StartupScript
	}
	for k, v := range c.Metadata {
		vCopy := v
		c.Instance.Metadata.Items = append(c.Instance.Metadata.Items, &compute.MetadataItems{Key: k, Value: &vCopy})
	}
	return nil
}

func (c *CreateInstance) populateNetworks() dErr {
	defaultAcs := []*compute.AccessConfig{{Type: defaultAccessConfigType}}
	defaultN := "default"

	if c.NetworkInterfaces == nil {
		c.NetworkInterfaces = []*compute.NetworkInterface{{}}
	}
	for _, n := range c.NetworkInterfaces {
		if n.AccessConfigs == nil {
			n.AccessConfigs = defaultAcs
		}
		n.Network = strOr(n.Network, defaultN)
		if networkURLRegex.MatchString(n.Network) {
			n.Network = extendPartialURL(n.Network, c.Project)
		} else {
			n.Network = fmt.Sprintf("projects/%s/global/networks/%s", c.Project, n.Network)
		}
	}

	return nil
}

func (c *CreateInstance) populateScopes() dErr {
	if len(c.Scopes) == 0 {
		c.Scopes = append(c.Scopes, "https://www.googleapis.com/auth/devstorage.read_only")
	}
	if c.ServiceAccounts == nil {
		c.ServiceAccounts = []*compute.ServiceAccount{{Email: "default", Scopes: c.Scopes}}
	}
	return nil
}

// populate preprocesses fields: Name, Project, Zone, Description, MachineType, NetworkInterfaces, Scopes, ServiceAccounts, and daisyName.
// - sets defaults
// - extends short partial URLs to include "projects/<project>"
func (c *CreateInstances) populate(ctx context.Context, s *Step) dErr {
	var errs dErr
	for _, ci := range *c {
		// General fields preprocessing.
		ci.daisyName = ci.Name
		if ci.ExactName && ci.RealName == "" {
			ci.RealName = ci.Name
		}
		if ci.RealName != "" {
			ci.Name = ci.RealName
		} else {
			ci.Name = s.w.genName(ci.Name)
		}
		ci.Project = strOr(ci.Project, s.w.Project)
		ci.Zone = strOr(ci.Zone, s.w.Zone)
		ci.Description = strOr(ci.Description, fmt.Sprintf("Instance created by Daisy in workflow %q on behalf of %s.", s.w.Name, s.w.username))

		errs = addErrs(errs, ci.populateDisks(s.w))
		errs = addErrs(errs, ci.populateMachineType())
		errs = addErrs(errs, ci.populateMetadata(s.w))
		errs = addErrs(errs, ci.populateNetworks())
		errs = addErrs(errs, ci.populateScopes())
	}

	return errs
}

func (c *CreateInstance) validateDisks(s *Step) (errs dErr) {
	if len(c.Disks) == 0 {
		errs = addErrs(errs, errf("cannot create instance: no disks provided"))
	}

	for _, d := range c.Disks {
		if !checkDiskMode(d.Mode) {
			errs = addErrs(errs, errf("cannot create instance: bad disk mode: %q", d.Mode))
		}
		if d.Source != "" && d.InitializeParams != nil {
			errs = addErrs(errs, errf("cannot create instance: disk.source and disk.initializeParams are mutually exclusive"))
		}
		if d.InitializeParams != nil {
			errs = addErrs(errs, c.validateDiskInitializeParams(d, s))
		} else {
			errs = addErrs(errs, c.validateDiskSource(d, s))
		}
	}
	return
}

func (c *CreateInstance) validateDiskSource(d *compute.AttachedDisk, s *Step) (errs dErr) {
	dr, err := disks[s.w].registerUsage(d.Source, s)
	if err != nil {
		errs = addErrs(errs, err)
		return
	}

	// Ensure disk is in the same project and zone.
	result := namedSubexp(diskURLRgx, dr.link)
	if result["project"] != c.Project {
		errs = addErrs(errs, errf("cannot create instance in project %q with disk in project %q: %q", c.Project, result["project"], d.Source))
	}
	if result["zone"] != c.Zone {
		errs = addErrs(errs, errf("cannot create instance in project %q with disk in zone %q: %q", c.Zone, result["zone"], d.Source))
	}
	return
}

func (c *CreateInstance) validateDiskInitializeParams(d *compute.AttachedDisk, s *Step) (errs dErr) {
	p := d.InitializeParams
	if !rfc1035Rgx.MatchString(p.DiskName) {
		errs = addErrs(errs, errf("cannot create instance: bad InitializeParams.DiskName: %q", p.DiskName))
	}
	if _, err := images[s.w].registerUsage(p.SourceImage, s); err != nil {
		errs = addErrs(errs, errf("cannot create instance: can't use InitializeParams.SourceImage %q: %v", p.SourceImage, err))
	}
	parts := namedSubexp(diskTypeURLRgx, p.DiskType)
	if parts["project"] != c.Project {
		errs = addErrs(errs, errf("cannot create instance in project %q with InitializeParams.DiskType in project %q", c.Project, parts["project"]))
	}
	if parts["zone"] != c.Zone {
		errs = addErrs(errs, errf("cannot create instance in zone %q with InitializeParams.DiskType in zone %q", c.Zone, parts["zone"]))
	}

	link := fmt.Sprintf("projects/%s/zones/%s/disks/%s", c.Project, c.Zone, p.DiskName)
	// Set cleanup if not being autodeleted.
	r := &resource{real: p.DiskName, link: link, noCleanup: d.AutoDelete}
	errs = addErrs(errs, disks[s.w].registerCreation(p.DiskName, r, s, false))
	return
}

func (c *CreateInstance) validateMachineType(client daisyCompute.Client) (errs dErr) {
	if !machineTypeURLRegex.MatchString(c.MachineType) {
		errs = addErrs(errs, errf("can't create instance: bad MachineType: %q", c.MachineType))
		return
	}

	result := namedSubexp(machineTypeURLRegex, c.MachineType)
	if result["project"] != c.Project {
		errs = addErrs(errs, errf("cannot create instance in project %q with MachineType in project %q: %q", c.Project, result["project"], c.MachineType))
	}
	if result["zone"] != c.Zone {
		errs = addErrs(errs, errf("cannot create instance in zone %q with MachineType in zone %q: %q", c.Zone, result["zone"], c.MachineType))
	}

	if exists, err := machineTypeExists(client, result["project"], result["zone"], result["machinetype"]); err != nil {
		errs = addErrs(errs, errf("cannot create instance, bad machineType lookup: %q, error: %v", result["machinetype"], err))
	} else if !exists {
		errs = addErrs(errs, errf("cannot create instance, machineType does not exist: %q", result["machinetype"]))
	}
	return
}

func (c *CreateInstance) validateNetworks(s *Step) (errs dErr) {
	for _, n := range c.NetworkInterfaces {
		nr, err := networks[s.w].registerUsage(n.Network, s)
		if err != nil {
			errs = addErrs(errs, err)
			return
		}

		// Ensure network is in the same project.
		result := namedSubexp(networkURLRegex, nr.link)
		if result["project"] != c.Project {
			errs = addErrs(errs, errf("cannot create instance in project %q with Network in project %q: %q", c.Project, result["project"], n.Network))
		}

	}
	return
}

func (c *CreateInstances) validate(ctx context.Context, s *Step) dErr {
	var errs dErr
	for _, ci := range *c {
		if !checkName(ci.Name) {
			errs = addErrs(errs, errf("cannot create instance %q: bad name", ci.Name))
		}

		if exists, err := projectExists(s.w.ComputeClient, ci.Project); err != nil {
			return errf("cannot create instance: bad project lookup: %q, error: %v", ci.Project, err)
		} else if !exists {
			return errf("cannot create instance: project does not exist: %q", ci.Project)
		}

		if exists, err := zoneExists(s.w.ComputeClient, ci.Project, ci.Zone); err != nil {
			return errf("cannot create instance: bad zone lookup: %q, error: %v", ci.Zone, err)
		} else if !exists {
			return errf("cannot create instance: zone does not exist: %q", ci.Zone)
		}

		errs = addErrs(errs, ci.validateDisks(s))
		errs = addErrs(errs, ci.validateMachineType(s.w.ComputeClient))
		errs = addErrs(errs, ci.validateNetworks(s))

		// Register creation.
		link := fmt.Sprintf("projects/%s/zones/%s/instances/%s", ci.Project, ci.Zone, ci.Name)
		r := &resource{real: ci.Name, link: link, noCleanup: ci.NoCleanup}
		errs = addErrs(errs, instances[s.w].registerCreation(ci.daisyName, r, s))
	}

	return errs
}

func (c *CreateInstances) run(ctx context.Context, s *Step) dErr {
	var wg sync.WaitGroup
	w := s.w
	eChan := make(chan dErr)
	for _, ci := range *c {
		wg.Add(1)
		go func(ci *CreateInstance) {
			defer wg.Done()

			for _, d := range ci.Disks {
				if diskRes, ok := disks[w].get(d.Source); ok {
					d.Source = diskRes.link
				}
			}

			w.logger.Printf("CreateInstances: creating instance %q.", ci.Name)
			if err := w.ComputeClient.CreateInstance(ci.Project, ci.Zone, &ci.Instance); err != nil {
				eChan <- newErr(err)
				return
			}
			go logSerialOutput(ctx, w, ci.Name, 1, 3*time.Second)
		}(ci)
	}

	go func() {
		wg.Wait()
		eChan <- nil
	}()

	select {
	case err := <-eChan:
		return err
	case <-w.Cancel:
		// Wait so instances being created now can be deleted.
		wg.Wait()
		return nil
	}
}
