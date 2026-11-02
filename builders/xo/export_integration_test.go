//go:build integration

package xo

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// FindTemplate returns the id of the template named name.
func FindTemplate(ctx context.Context, c *Client, name string) (string, error) {
	b := &Builder{client: c}
	o, err := b.find(ctx, "vm-templates", "template", name, "")
	if err != nil {
		return "", err
	}
	return o.ID, nil
}

// DeleteTemplate deletes a template and its disks.
func DeleteTemplate(ctx context.Context, c *Client, id string) error {
	return c.do(ctx, http.MethodDelete, "/vm-templates/"+url.PathEscape(id), nil, nil)
}

// TemplatesWithPrefix returns the names of the templates starting with
// prefix.
func TemplatesWithPrefix(ctx context.Context, c *Client, prefix string) ([]string, error) {
	objs, err := c.list(ctx, "vm-templates")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, o := range objs {
		if strings.HasPrefix(o.NameLabel, prefix) {
			names = append(names, o.NameLabel)
		}
	}
	return names, nil
}
