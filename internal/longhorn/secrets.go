package longhorn

import (
	"context"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/metadata"
)

var gvrSecret = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

// SecretNames lists the secret names in the Longhorn namespace (for the
// credential secret of a backup target). It uses the metadata API: only
// names and labels travel, never the secret data.
func (c *Client) SecretNames(ctx context.Context) ([]string, error) {
	if c.meta == nil {
		m, err := metadata.NewForConfig(c.Cluster.Config)
		if err != nil {
			return nil, err
		}
		c.meta = m
	}
	l, err := c.meta.Resource(gvrSecret).Namespace(c.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(l.Items))
	for _, it := range l.Items {
		names = append(names, it.Name)
	}
	sort.Strings(names)
	return names, nil
}

// SetMetadataClient replaces the metadata client (demo mode, tests).
func (c *Client) SetMetadataClient(m metadata.Interface) { c.meta = m }
