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
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"cloud.google.com/go/storage"
	daisyCompute "github.com/GoogleCloudPlatform/compute-image-tools/daisy/compute"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
)

type mockStep struct {
	populateImpl func(context.Context, *Step) dErr
	runImpl      func(context.Context, *Step) dErr
	validateImpl func(context.Context, *Step) dErr
}

func (m *mockStep) populate(ctx context.Context, s *Step) dErr {
	if m.populateImpl != nil {
		return m.populateImpl(ctx, s)
	}
	return nil
}

func (m *mockStep) run(ctx context.Context, s *Step) dErr {
	if m.runImpl != nil {
		return m.runImpl(ctx, s)
	}
	return nil
}

func (m *mockStep) validate(ctx context.Context, s *Step) dErr {
	if m.validateImpl != nil {
		return m.validateImpl(ctx, s)
	}
	return nil
}

var (
	testWf          = "test-wf"
	testProject     = "test-project"
	testZone        = "test-zone"
	testDisk        = "test-disk"
	testImage       = "test-image"
	testMachineType = "test-machine-type"
	testLicense     = "test-license"
	testNetwork     = "test-network"
	testFamily      = "test-family"
	testGCSPath     = "gs://test-bucket"
	testGCSObjs     []string
	testGCSObjsMx   = sync.Mutex{}
)

func testWorkflow() *Workflow {
	w := New()
	w.id = "abcdef"
	w.Name = testWf
	w.GCSPath = testGCSPath
	w.Project = testProject
	w.Zone = testZone
	w.ComputeClient, _ = newTestGCEClient()
	w.StorageClient, _ = newTestGCSClient()
	w.Cancel = make(chan struct{})
	w.logger = log.New(ioutil.Discard, "", 0)
	return w
}

func addGCSObj(o string) {
	testGCSObjsMx.Lock()
	defer testGCSObjsMx.Unlock()
	testGCSObjs = append(testGCSObjs, o)
}

func newTestGCEClient() (*daisyCompute.TestClient, error) {
	_, c, err := daisyCompute.NewTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.Contains(r.URL.String(), "serialPort?alt=json&port=1") {
			fmt.Fprintln(w, `{"Contents":"failsuccess","Start":"0"}`)
		} else if r.Method == "GET" && strings.Contains(r.URL.String(), "serialPort?alt=json&port=2") {
			fmt.Fprintln(w, `{"Contents":"successfail","Start":"0"}`)
		} else {
			fmt.Fprintln(w, `{"Status":"DONE","SelfLink":"link"}`)
		}
	}))

	c.GetProjectFn = func(project string) (*compute.Project, error) {
		if project == testProject {
			return nil, nil
		}
		return nil, errors.New("bad project")
	}
	c.GetZoneFn = func(_, zone string) (*compute.Zone, error) {
		if zone == testZone {
			return nil, nil
		}
		return nil, errors.New("bad zone")
	}
	c.GetMachineTypeFn = func(_, _, mt string) (*compute.MachineType, error) {
		if mt == testMachineType {
			return nil, nil
		}
		return nil, errors.New("bad machinetype")
	}
	c.ListMachineTypesFn = func(p, z string) ([]*compute.MachineType, error) {
		if p != testProject {
			return nil, errors.New("bad project: " + p)
		}
		if z != testZone {
			return nil, errors.New("bad zone: " + z)
		}
		return []*compute.MachineType{{Name: testMachineType}}, nil
	}
	c.ListZonesFn = func(_ string) ([]*compute.Zone, error) {
		return []*compute.Zone{{Name: testZone}}, nil
	}
	c.ListImagesFn = func(_ string) ([]*compute.Image, error) {
		return []*compute.Image{{Name: testImage}}, nil
	}
	c.ListDisksFn = func(p, z string) ([]*compute.Disk, error) {
		if p != testProject {
			return nil, errors.New("bad project: " + p)
		}
		if z != testZone {
			return nil, errors.New("bad zone: " + z)
		}
		return []*compute.Disk{{Name: testDisk}}, nil
	}
	c.GetLicenseFn = func(p, l string) (*compute.License, error) {
		if p != testProject {
			return nil, errors.New("bad project: " + p)
		}
		if l != testLicense {
			return nil, errors.New("bad license: " + l)
		}
		return nil, nil
	}
	c.ListNetworksFn = func(p string) ([]*compute.Network, error) {
		if p != testProject {
			return nil, errors.New("bad project: " + p)
		}
		return []*compute.Network{{Name: testNetwork}}, nil
	}
	c.CreateImageFn = func(p string, i *compute.Image) error {
		if p != testProject {
			return errors.New("bad project: " + p)
		}
		if i.Name != testImage {
			return errors.New("bad image name: " + i.Name)
		}
		if i.SourceDisk != "" && i.SourceDisk != testDisk {
			return errors.New("bad source disk: " + i.SourceDisk)
		}
		return nil
	}

	return c, err
}

func newTestGCSClient() (*storage.Client, error) {
	nameRgx := regexp.MustCompile(`"name":"([^"].*)"`)
	rewriteRgx := regexp.MustCompile(`/b/([^/]+)/o/([^/]+)/rewriteTo/b/([^/]+)/o/([^?]+)`)
	uploadRgx := regexp.MustCompile(`/b/([^/]+)/o?.*uploadType=multipart.*`)
	getObjRgx := regexp.MustCompile(`/b/.+/o/.+alt=json&projection=full`)
	listObjsRgx := regexp.MustCompile(`/b/.+/o\?alt=json&delimiter=&pageToken=&prefix=.+&projection=full&versions=false`)
	listObjsNoPrefixRgx := regexp.MustCompile(`/b/.+/o\?alt=json&delimiter=&pageToken=&prefix=&projection=full&versions=false`)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		u := r.URL.String()
		m := r.Method

		if match := uploadRgx.FindStringSubmatch(u); m == "POST" && match != nil {
			body, _ := ioutil.ReadAll(r.Body)
			n := nameRgx.FindStringSubmatch(string(body))[1]
			addGCSObj(n)
			fmt.Fprintf(w, `{"kind":"storage#object","bucket":"%s","name":"%s"}`, match[1], n)
		} else if match := rewriteRgx.FindStringSubmatch(u); m == "POST" && match != nil {
			if strings.Contains(match[1], "dne") || strings.Contains(match[2], "dne") {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, storage.ErrObjectNotExist)
				return
			}
			path, err := url.PathUnescape(match[4])
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, err)
				return
			}
			addGCSObj(path)
			o := fmt.Sprintf(`{"bucket":"%s","name":"%s"}`, match[3], match[4])
			fmt.Fprintf(w, `{"kind": "storage#rewriteResponse", "done": true, "objectSize": "1", "totalBytesRewritten": "1", "resource": %s}`, o)
		} else if match := getObjRgx.FindStringSubmatch(u); m == "GET" && match != nil {
			// Return StatusNotFound for objects that do not exist.
			if strings.Contains(match[0], "dne") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			// Yes this object exists, we don't need to fill out the values, just return something.
			fmt.Fprint(w, "{}")
		} else if match := listObjsRgx.FindStringSubmatch(u); m == "GET" && match != nil {
			// Return StatusNotFound for objects that do not exist.
			if strings.Contains(match[0], "dne") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			// Return 2 objects for testing recursiveGCS.
			fmt.Fprint(w, `{"kind": "storage#objects", "items": [{"kind": "storage#object", "name": "folder/object", "size": "1"},{"kind": "storage#object", "name": "folder/folder/object", "size": "1"}]}`)
		} else if match := listObjsNoPrefixRgx.FindStringSubmatch(u); m == "GET" && match != nil {
			// Return 2 objects for testing recursiveGCS.
			fmt.Fprint(w, `{"kind": "storage#objects", "items": [{"kind": "storage#object", "name": "object", "size": "1"},{"kind": "storage#object", "name": "folder/object", "size": "1"}]}`)
		} else if m == "PUT" && u == "/b/bucket/o/object/acl/allUsers?alt=json" {
			fmt.Fprint(w, `{}`)
		} else if m == "PUT" && u == "/b/bucket/o/object%2Ffolder%2Ffolder%2Fobject/acl/allUsers?alt=json" {
			fmt.Fprint(w, `{}`)
		} else if m == "PUT" && u == "/b/bucket/o/object%2Ffolder%2Fobject/acl/allUsers?alt=json" {
			fmt.Fprint(w, `{}`)
		} else if m == "GET" && u == "/b?alt=json&pageToken=&prefix=&project=foo-project&projection=full" {
			fmt.Fprint(w, `{}`)
		} else if m == "GET" && u == "/b?alt=json&pageToken=&prefix=&project=bar-project&projection=full" {
			fmt.Fprint(w, `{"items": [{"name": "bar-project-daisy-bkt"}]}`)
		} else if m == "POST" && u == "/b?alt=json&project=foo-project" {
			fmt.Fprint(w, `{}`)
		} else {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "testGCSClient unknown request: %+v\n", r)
		}
	}))

	return storage.NewClient(context.Background(), option.WithEndpoint(ts.URL), option.WithHTTPClient(http.DefaultClient))
}
