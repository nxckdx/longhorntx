package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	yamlv3 "go.yaml.in/yaml/v3"
)

// Editing the config file from the TUI settings page. The file is changed
// through a YAML node tree, so comments and layout the user wrote stay.

// Keys that can be set per context or in defaults.
var Keys = []string{"refresh", "autoPollRate", "readOnly", "theme", "color", "trafficBudget", "overview", "namespace"}

// ErrManaged: the file is a symlink into the Nix store (Home Manager) or
// otherwise not writable – it has to be changed where it comes from.
var ErrManaged = errors.New("the config file is managed elsewhere (e.g. Home Manager) and read-only – change it there")

// Set writes key=value into defaults (ctx "") or contexts.<ctx>; value ""
// removes the key so the value is inherited again. The result is parsed
// before writing, so an invalid value never reaches the file.
func Set(path, ctx, key, value string) error {
	if !slices.Contains(Keys, key) {
		return fmt.Errorf("unknown setting %q", key)
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		if dst, _ := filepath.EvalSymlinks(path); strings.HasPrefix(dst, "/nix/store/") {
			return ErrManaged
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var doc yamlv3.Node
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := yamlv3.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	if doc.Kind == 0 {
		doc = yamlv3.Node{Kind: yamlv3.DocumentNode, Content: []*yamlv3.Node{{Kind: yamlv3.MappingNode}}}
	}
	root := doc.Content[0]
	if root.Kind != yamlv3.MappingNode {
		return fmt.Errorf("%s: top level is not a mapping", path)
	}
	var target *yamlv3.Node
	if ctx == "" {
		target = child(root, "defaults", value != "")
	} else if cs := child(root, "contexts", value != ""); cs != nil {
		target = child(cs, ctx, value != "")
	}
	if target == nil { // removing from a section that does not exist
		return nil
	}
	if value == "" {
		removeKey(target, key)
	} else {
		setScalar(target, key, value)
	}
	var out bytes.Buffer
	enc := yamlv3.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := Parse(out.Bytes(), &File{}); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out.Bytes(), 0o644); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return ErrManaged
		}
		return err
	}
	return os.Rename(tmp, path)
}

// child returns the mapping under key, creating it (block style) if asked.
func child(m *yamlv3.Node, key string, create bool) *yamlv3.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v := m.Content[i+1]
			if v.Kind != yamlv3.MappingNode {
				if !create {
					return nil
				}
				*v = yamlv3.Node{Kind: yamlv3.MappingNode}
			}
			if create {
				v.Style = 0 // "contexts: {}" becomes a block mapping
			}
			return v
		}
	}
	if !create {
		return nil
	}
	m.Style = 0
	v := &yamlv3.Node{Kind: yamlv3.MappingNode}
	m.Content = append(m.Content, &yamlv3.Node{Kind: yamlv3.ScalarNode, Value: key}, v)
	return v
}

func setScalar(m *yamlv3.Node, key, value string) {
	val := &yamlv3.Node{Kind: yamlv3.ScalarNode, Value: value, Tag: "!!str"}
	switch key {
	case "readOnly", "overview":
		if b, err := strconv.ParseBool(value); err == nil {
			val.Tag, val.Value = "!!bool", strconv.FormatBool(b)
		}
	case "autoPollRate":
		if _, err := strconv.Atoi(value); err == nil {
			val.Tag = "!!int"
		}
	}
	m.Style = 0
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			val.HeadComment, val.LineComment = m.Content[i+1].HeadComment, m.Content[i+1].LineComment
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content, &yamlv3.Node{Kind: yamlv3.ScalarNode, Value: key}, val)
}

func removeKey(m *yamlv3.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}
