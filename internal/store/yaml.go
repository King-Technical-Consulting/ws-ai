package store

import "gopkg.in/yaml.v3"

func yamlOf(v any) (string, error) {
	b, err := yaml.Marshal(v)
	return string(b), err
}
