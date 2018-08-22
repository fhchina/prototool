// Copyright (c) 2018 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package settings

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/uber/prototool/internal/strs"
	"go.uber.org/zap"
	"gopkg.in/yaml.v2"
)

type configProvider struct {
	logger *zap.Logger
}

func newConfigProvider(options ...ConfigProviderOption) *configProvider {
	configProvider := &configProvider{
		logger: zap.NewNop(),
	}
	for _, option := range options {
		option(configProvider)
	}
	return configProvider
}

func (c *configProvider) GetForDir(dirPath string) (Config, error) {
	filePath, err := c.GetFilePathForDir(dirPath)
	if err != nil {
		return Config{}, err
	}
	if filePath == "" {
		return Config{}, nil
	}
	return c.Get(filePath)
}

func (c *configProvider) GetFilePathForDir(dirPath string) (string, error) {
	if !filepath.IsAbs(dirPath) {
		return "", fmt.Errorf("%s is not an absolute path", dirPath)
	}
	dirPath = filepath.Clean(dirPath)
	filePath, _ := getFilePathForDir(dirPath)
	return filePath, nil
}

func (c *configProvider) Get(filePath string) (Config, error) {
	if !filepath.IsAbs(filePath) {
		return Config{}, fmt.Errorf("%s is not an absolute path", filePath)
	}
	filePath = filepath.Clean(filePath)
	return get(filePath)
}

func (c *configProvider) GetExcludePrefixesForDir(dirPath string) ([]string, error) {
	if !filepath.IsAbs(dirPath) {
		return nil, fmt.Errorf("%s is not an absolute path", dirPath)
	}
	dirPath = filepath.Clean(dirPath)
	return getExcludePrefixesForDir(dirPath)
}

// getFilePathForDir tries to find a file named DefaultConfigFilename starting in the
// given directory, and going up a directory until hitting root.
//
// The directory must be an absolute path.
//
// If no such file is found, "" is returned.
// Also returns all the directories this Config applies to.
func getFilePathForDir(dirPath string) (string, []string) {
	var dirPaths []string
	for {
		dirPaths = append(dirPaths, dirPath)
		filePath := filepath.Join(dirPath, DefaultConfigFilename)
		if _, err := os.Stat(filePath); err == nil {
			return filePath, dirPaths
		}
		if dirPath == "/" {
			return "", dirPaths
		}
		dirPath = filepath.Dir(dirPath)
	}
}

// get reads the config at the given path.
//
// This is expected to be in YAML format.
func get(filePath string) (Config, error) {
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return Config{}, err
	}
	externalConfig := ExternalConfig{}
	if err := yaml.UnmarshalStrict(data, &externalConfig); err != nil {
		return Config{}, err
	}
	return externalConfigToConfig(externalConfig, filepath.Dir(filePath))
}

// externalConfigToConfig converts an ExternalConfig to a Config.
//
// This will return a valid Config, or an error.
func externalConfigToConfig(e ExternalConfig, dirPath string) (Config, error) {
	excludePrefixes, err := getExcludePrefixes(e.Excludes, dirPath)
	if err != nil {
		return Config{}, err
	}
	includePaths := make([]string, 0, len(e.Protoc.Includes))
	for _, includePath := range strs.DedupeSort(e.Protoc.Includes, nil) {
		if !filepath.IsAbs(includePath) {
			includePath = filepath.Join(dirPath, includePath)
		}
		includePath = filepath.Clean(includePath)
		includePaths = append(includePaths, includePath)
	}
	ignoreIDToFilePaths := make(map[string][]string)
	for _, ignore := range e.Lint.Ignores {
		id := strings.ToUpper(ignore.ID)
		for _, protoFilePath := range ignore.Files {
			if !filepath.IsAbs(protoFilePath) {
				protoFilePath = filepath.Join(dirPath, protoFilePath)
			}
			protoFilePath = filepath.Clean(protoFilePath)
			if _, ok := ignoreIDToFilePaths[id]; !ok {
				ignoreIDToFilePaths[id] = make([]string, 0)
			}
			ignoreIDToFilePaths[id] = append(ignoreIDToFilePaths[id], protoFilePath)
		}
	}

	genPlugins := make([]GenPlugin, len(e.Gen.Plugins))
	for i, plugin := range e.Gen.Plugins {
		genPluginType, err := ParseGenPluginType(plugin.Type)
		if err != nil {
			return Config{}, err
		}
		if plugin.Output == "" {
			return Config{}, fmt.Errorf("output path required for plugin %s", plugin.Name)
		}
		var relPath, absPath string
		if filepath.IsAbs(plugin.Output) {
			absPath = filepath.Clean(plugin.Output)
			relPath, err = filepath.Rel(dirPath, absPath)
			if err != nil {
				return Config{}, fmt.Errorf("failed to resolve plugin %q output absolute path %q to a relative path with base %q: %v", plugin.Name, absPath, dirPath, err)
			}
		} else {
			relPath = plugin.Output
			absPath = filepath.Clean(filepath.Join(dirPath, relPath))
		}
		genPlugins[i] = GenPlugin{
			Name:  plugin.Name,
			Path:  plugin.Path,
			Type:  genPluginType,
			Flags: plugin.Flags,
			OutputPath: OutputPath{
				RelPath: relPath,
				AbsPath: absPath,
			},
		}
	}
	sort.Slice(genPlugins, func(i int, j int) bool { return genPlugins[i].Name < genPlugins[j].Name })

	createDirPathToBasePackage := make(map[string]string)
	for _, pkg := range e.Create.Packages {
		relDirPath := pkg.Directory
		basePackage := pkg.Name
		if relDirPath == "" {
			return Config{}, fmt.Errorf("directory for create package is empty")
		}
		if basePackage == "" {
			return Config{}, fmt.Errorf("name for create package is empty")
		}
		if filepath.IsAbs(relDirPath) {
			return Config{}, fmt.Errorf("directory for create package must be relative: %s", relDirPath)
		}
		createDirPathToBasePackage[filepath.Clean(filepath.Join(dirPath, relDirPath))] = basePackage
	}
	// to make testing easier
	if len(createDirPathToBasePackage) == 0 {
		createDirPathToBasePackage = nil
	}

	config := Config{
		DirPath:         dirPath,
		ExcludePrefixes: excludePrefixes,
		Compile: CompileConfig{
			ProtobufVersion:               e.Protoc.Version,
			IncludePaths:                  includePaths,
			IncludeWellKnownTypes:         true, // Always include the well-known types.
			AllowUnusedImports:            e.Protoc.AllowUnusedImports,
			IncludeExtendedWellKnownTypes: e.Protoc.IncludeExtendedWKT,
		},
		Create: CreateConfig{
			DirPathToBasePackage: createDirPathToBasePackage,
		},
		Lint: LintConfig{
			IncludeIDs:          strs.DedupeSort(e.Lint.Rules.Add, strings.ToUpper),
			ExcludeIDs:          strs.DedupeSort(e.Lint.Rules.Remove, strings.ToUpper),
			NoDefault:           e.Lint.Rules.NoDefault,
			IgnoreIDToFilePaths: ignoreIDToFilePaths,
		},
		Gen: GenConfig{
			GoPluginOptions: GenGoPluginOptions{
				ImportPath:     e.Gen.GoOptions.ImportPath,
				ExtraModifiers: e.Gen.GoOptions.ExtraModifiers,
			},
			Plugins: genPlugins,
		},
	}

	for _, genPlugin := range config.Gen.Plugins {
		// TODO: technically protoc-gen-protoc-gen-foo is a valid
		// plugin binary with name protoc-gen-foo, but do we want
		// to error if protoc-gen- is a prefix of a name?
		// I think this will be a common enough mistake that we
		// can remove this later. Or, do we want names to include
		// the protoc-gen- part?
		if strings.HasPrefix(genPlugin.Name, "protoc-gen-") {
			return Config{}, fmt.Errorf("plugin name provided was %s, do not include the protoc-gen- prefix", genPlugin.Name)
		}
		if _, ok := _genPluginTypeToString[genPlugin.Type]; !ok {
			return Config{}, fmt.Errorf("unknown GenPluginType: %v", genPlugin.Type)
		}
		if (genPlugin.Type.IsGo() || genPlugin.Type.IsGogo()) && config.Gen.GoPluginOptions.ImportPath == "" {
			return Config{}, fmt.Errorf("go plugin %s specified but no import path provided", genPlugin.Name)
		}
	}

	if intersection := strs.Intersection(config.Lint.IncludeIDs, config.Lint.ExcludeIDs); len(intersection) > 0 {
		return Config{}, fmt.Errorf("config had intersection of %v between lint_include and lint_exclude", intersection)
	}
	return config, nil
}

func getExcludePrefixesForDir(dirPath string) ([]string, error) {
	filePath := filepath.Join(dirPath, DefaultConfigFilename)
	if _, err := os.Stat(filePath); err != nil {
		return []string{}, nil
	}
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	s := struct {
		ExcludePaths []string `json:"excludes,omitempty" yaml:"excludes,omitempty"`
	}{}
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return getExcludePrefixes(s.ExcludePaths, dirPath)
}

func getExcludePrefixes(excludes []string, dirPath string) ([]string, error) {
	excludePrefixes := make([]string, 0, len(excludes))
	for _, excludePrefix := range strs.DedupeSort(excludes, nil) {
		if !filepath.IsAbs(excludePrefix) {
			excludePrefix = filepath.Join(dirPath, excludePrefix)
		}
		excludePrefix = filepath.Clean(excludePrefix)
		if excludePrefix == dirPath {
			return nil, fmt.Errorf("cannot exclude directory of config file: %s", dirPath)
		}
		if !strings.HasPrefix(excludePrefix, dirPath) {
			return nil, fmt.Errorf("cannot exclude directory outside of config file directory %s: %s", dirPath, excludePrefix)
		}
		excludePrefixes = append(excludePrefixes, excludePrefix)
	}
	return excludePrefixes, nil
}
