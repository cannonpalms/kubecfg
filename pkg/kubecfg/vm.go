// Copyright 2023 The kubecfg authors
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

package kubecfg

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/genuinetools/reg/registry"
	"github.com/google/go-jsonnet"
	"github.com/kubecfg/kubecfg/internal/acquire"
	"github.com/kubecfg/kubecfg/pkg/kubecfg/vars"
	"github.com/kubecfg/kubecfg/utils"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type jsonnetVMOpts struct {
	alpha      bool
	workingDir string
	importPath []string
	importURLs []string
	vars       []vars.Var

	resolverType          ResolverType
	resolverFailureAction ResolverFailureAction
}

type JsonnetVMOpt func(*jsonnetVMOpts)

func WithAlpha(enable bool) JsonnetVMOpt {
	return func(opts *jsonnetVMOpts) {
		opts.alpha = enable
	}
}

func WithWorkingDir(dir string) JsonnetVMOpt {
	return func(opts *jsonnetVMOpts) {
		opts.workingDir = dir
	}
}

func WithImportPath(importPath ...string) JsonnetVMOpt {
	return func(opts *jsonnetVMOpts) {
		opts.importPath = importPath
	}
}

func WithImportURLs(importURLs ...string) JsonnetVMOpt {
	return func(opts *jsonnetVMOpts) {
		opts.importURLs = importURLs
	}
}

func WithVar(v vars.Var) JsonnetVMOpt {
	return func(opts *jsonnetVMOpts) {
		opts.vars = append(opts.vars, v)
	}
}

type ResolverType int

const (
	NoopResolver ResolverType = iota
	RegistryResolver
)

type ResolverFailureAction int

const (
	IgnoreResolverError ResolverFailureAction = iota
	WarnResolverError
	ReportResolverError
)

func WithResolver(typ ResolverType, failureMode ResolverFailureAction) JsonnetVMOpt {
	return func(opts *jsonnetVMOpts) {
		opts.resolverType = typ
		opts.resolverFailureAction = failureMode
	}
}

// JsonnetVM constructs a new jsonnet.VM, according to command line
// flags
func JsonnetVM(opt ...JsonnetVMOpt) (*jsonnet.VM, error) {
	vm := jsonnet.MakeVM()

	var opts jsonnetVMOpts
	for _, o := range opt {
		o(&opts)
	}

	var searchUrls []*url.URL
	for _, p := range opts.importPath {
		p, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		searchUrls = append(searchUrls, dirURL(p))
	}

	sURLs := opts.importURLs
	// Special URL scheme used to find embedded content
	sURLs = append(sURLs, "internal:///")

	for _, ustr := range sURLs {
		u, err := url.Parse(ustr)
		if err != nil {
			return nil, err
		}
		if u.Path[len(u.Path)-1] != '/' {
			u.Path = u.Path + "/"
		}
		searchUrls = append(searchUrls, u)
	}

	for _, u := range searchUrls {
		log.Debugln("Jsonnet search path:", u)
	}

	if opts.workingDir == "" {
		var err error
		opts.workingDir, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("unable to determine current working directory: %w", err)
		}
	}

	cwd := opts.workingDir
	for _, v := range opts.vars {
		name, value := v.Name, v.Value

		if v.Source == vars.File {
			// Ensure that the import path we construct here is absolute, so that our Importer
			// won't try to glean from an extVar or TLA reference the context necessary to
			// resolve a relative path.
			path := value
			if !filepath.IsAbs(path) {
				path = filepath.Join(cwd, path)
			}
			u := &url.URL{Scheme: "file", Path: path}
			var imp string
			if v.Expr == vars.Code {
				imp = "import"
			} else {
				imp = "importstr"
			}

			value = fmt.Sprintf("%s @'%s'", imp, strings.ReplaceAll(u.String(), "'", "''"))
		}

		v.Setter()(vm, name, value)
	}

	vm.Importer(utils.MakeUniversalImporter(searchUrls, opts.alpha))

	resolver, err := buildResolver(&opts)
	if err != nil {
		return nil, err
	}
	utils.RegisterNativeFuncs(vm, resolver)

	return vm, nil
}

func buildResolver(opts *jsonnetVMOpts) (utils.Resolver, error) {
	ret := resolverErrorWrapper{}

	switch action := opts.resolverFailureAction; action {
	case IgnoreResolverError:
		ret.OnErr = func(error) error { return nil }
	case WarnResolverError:
		ret.OnErr = func(err error) error {
			log.Warning(err.Error())
			return nil
		}
	case ReportResolverError:
		ret.OnErr = func(err error) error { return err }
	default:
		return nil, fmt.Errorf("bad value %d for resolver failure mode", action)
	}

	switch resolver := opts.resolverType; resolver {
	case NoopResolver:
		ret.Inner = utils.NewIdentityResolver()
	case RegistryResolver:
		ret.Inner = utils.NewRegistryResolver(registry.Opt{})
	default:
		return nil, fmt.Errorf("bad value %d for resolver tyoe", resolver)
	}

	return &ret, nil
}

type resolverErrorWrapper struct {
	Inner utils.Resolver
	OnErr func(error) error
}

func (r *resolverErrorWrapper) Resolve(image *utils.ImageName) error {
	err := r.Inner.Resolve(image)
	if err != nil {
		err = r.OnErr(err)
	}
	return err
}

// NB: `path` is assumed to be in native-OS path separator form
func dirURL(path string) *url.URL {
	path = filepath.ToSlash(path)
	if path[len(path)-1] != '/' {
		// trailing slash is important
		path = path + "/"
	}
	return &url.URL{Scheme: "file", Path: path}
}

// ReadObjects evaluates all jsonnet files in paths and return all the k8s objects found in it.
// Unlike utils.Read this checks for duplicates and flattens the v1 Lists.
func ReadObjects(vm *jsonnet.VM, paths []string, opts ...utils.ReadOption) ([]*unstructured.Unstructured, error) {
	opt := acquire.MakeReadOptions(opts)

	if overlay := opt.OverlayURL; overlay != "" {
		overlayExpression := func(url string) string {
			return utils.ToDataURL(fmt.Sprintf(`(import %q) + (import %q)`, url, overlay))
		}
		for i := range paths {
			paths[i] = overlayExpression(paths[i])
		}
	}
	if overlay := opt.OverlayCode; overlay != "" {
		overlayExpression := func(src string) string {
			return utils.ToDataURL(fmt.Sprintf(`(import %q) + (%s)`, src, overlay))
		}
		for i := range paths {
			paths[i] = overlayExpression(paths[i])
		}
	}

	res := []*unstructured.Unstructured{}
	for _, path := range paths {
		objs, err := utils.Read(vm, path, opts...)
		if err != nil {
			return nil, fmt.Errorf("error reading %s: %v", path, err)
		}

		res = append(res, utils.FlattenToV1(objs)...)
	}
	if err := utils.CheckDuplicates(res); err != nil {
		return nil, err
	}
	return res, nil
}
