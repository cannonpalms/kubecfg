// Copyright 2017 The kubecfg authors
//
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

package utils

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"

	jsonnet "github.com/google/go-jsonnet"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const (
	AnnotationProvenanceFile = "kubecfg.dev/provenance-file"
	AnnotationProvenancePath = "kubecfg.dev/provenance-path"
)

type readOptions struct {
	showProvenance bool
}

type ReadOption func(*readOptions)

func WithProvenance(show bool) ReadOption {
	return func(opts *readOptions) {
		opts.showProvenance = show
	}
}

// Read fetches and decodes K8s objects by path.
// TODO: Replace this with something supporting more sophisticated
// content negotiation.
func Read(vm *jsonnet.VM, path string, opts ...ReadOption) ([]runtime.Object, error) {
	var opt readOptions
	for _, o := range opts {
		o(&opt)
	}

	ext := filepath.Ext(path)
	if ext == ".json" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return jsonReader(f)
	} else if ext == ".yaml" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return yamlReader(f)
	} else if ext == ".jsonnet" {
		return jsonnetReader(vm, path, opt)
	}

	return nil, fmt.Errorf("Unknown file extension: %s", path)
}

func jsonReader(r io.Reader) ([]runtime.Object, error) {
	data, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}
	obj, _, err := unstructured.UnstructuredJSONScheme.Decode(data, nil, nil)
	if err != nil {
		return nil, err
	}
	return []runtime.Object{obj}, nil
}

func yamlReader(r io.ReadCloser) ([]runtime.Object, error) {
	decoder := yaml.NewYAMLReader(bufio.NewReader(r))
	ret := []runtime.Object{}
	for {
		bytes, err := decoder.Read()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if len(bytes) == 0 {
			continue
		}
		jsondata, err := yaml.ToJSON(bytes)
		if err != nil {
			return nil, err
		}
		obj, _, err := unstructured.UnstructuredJSONScheme.Decode(jsondata, nil, nil)
		if err != nil {
			return nil, err
		}
		ret = append(ret, obj)
	}
	return ret, nil
}

type walkContext struct {
	parent *walkContext
	label  string
	file   string
	opts   *readOptions
}

func (c *walkContext) path() string {
	parent := ""
	if c.parent != nil {
		parent = c.parent.path()
	}
	return parent + c.label
}

func (c *walkContext) child(label string) *walkContext {
	return &walkContext{
		parent: c,
		label:  label,
		file:   c.file,
		opts:   c.opts,
	}
}

func annotateProvenance(ctx *walkContext, o map[string]interface{}) {
	if _, found := o["metadata"]; !found {
		o["metadata"] = map[string]interface{}{}
	}
	if m, ok := o["metadata"].(map[string]interface{}); ok {
		if _, found := m["annotations"]; !found {
			m["annotations"] = map[string]interface{}{}
		}
		if a, ok := m["annotations"].(map[string]interface{}); ok {
			if file := ctx.file; file != "" {
				a[AnnotationProvenanceFile] = file
			}
			a[AnnotationProvenancePath] = ctx.path()
		}
	}
}

func jsonWalk(parentCtx *walkContext, obj interface{}) ([]interface{}, error) {
	switch o := obj.(type) {
	case nil:
		return []interface{}{}, nil
	case map[string]interface{}:
		if o["kind"] != nil && o["apiVersion"] != nil {
			if parentCtx.opts != nil && parentCtx.opts.showProvenance {
				annotateProvenance(parentCtx, o)
			}
			return []interface{}{o}, nil
		}
		ret := []interface{}{}
		for k, v := range o {
			children, err := jsonWalk(parentCtx.child(fmt.Sprintf(".%s", k)), v)
			if err != nil {
				return nil, err
			}
			ret = append(ret, children...)
		}
		return ret, nil
	case []interface{}:
		ret := make([]interface{}, 0, len(o))
		for i, v := range o {
			children, err := jsonWalk(parentCtx.child(fmt.Sprintf("[%d]", i)), v)
			if err != nil {
				return nil, err
			}
			ret = append(ret, children...)
		}
		return ret, nil
	default:
		return nil, fmt.Errorf("Looking for kubernetes object at %q, but instead found %T", parentCtx.path(), o)
	}
}

func jsonnetReader(vm *jsonnet.VM, path string, opts readOptions) ([]runtime.Object, error) {
	// TODO: Read via Importer, so we support HTTP, etc for first
	// file too.
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	pathUrl := &url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}

	bytes, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	jsonstr, err := vm.EvaluateSnippet(pathUrl.String(), string(bytes))
	if err != nil {
		return nil, err
	}

	log.Debugf("jsonnet result is: %s", jsonstr)

	var top interface{}
	if err = json.Unmarshal([]byte(jsonstr), &top); err != nil {
		return nil, err
	}

	objs, err := jsonWalk(&walkContext{file: path, label: "$", opts: &opts}, top)
	if err != nil {
		return nil, err
	}

	ret := make([]runtime.Object, 0, len(objs))
	for _, v := range objs {
		obj := &unstructured.Unstructured{Object: v.(map[string]interface{})}
		if obj.IsList() {
			// TODO: Use obj.ToList with newer apimachinery
			list := &unstructured.UnstructuredList{
				Object: obj.Object,
			}
			err := obj.EachListItem(func(item runtime.Object) error {
				castItem := item.(*unstructured.Unstructured)
				list.Items = append(list.Items, *castItem)
				return nil
			})
			if err != nil {
				return nil, err
			}
			ret = append(ret, list)
		} else {
			ret = append(ret, obj)
		}
	}

	return ret, nil
}

// FlattenToV1 expands any List-type objects into their members, and
// cooerces everything to v1.Unstructured.  Panics if coercion
// encounters an unexpected object type.
func FlattenToV1(objs []runtime.Object) []*unstructured.Unstructured {
	ret := make([]*unstructured.Unstructured, 0, len(objs))
	for _, obj := range objs {
		switch o := obj.(type) {
		case *unstructured.UnstructuredList:
			for i := range o.Items {
				ret = append(ret, &o.Items[i])
			}
		case *unstructured.Unstructured:
			ret = append(ret, o)
		default:
			panic("Unexpected unstructured object type")
		}
	}
	return ret
}
