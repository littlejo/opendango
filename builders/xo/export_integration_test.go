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

// TemplateMedia returns the names of the disks of a template and the
// number of its CD drives holding a disc.
func TemplateMedia(ctx context.Context, c *Client, id string) (disks []string, cds int, err error) {
	var vbds []struct {
		VDI string `json:"VDI"`
		CD  bool   `json:"is_cd_drive"`
	}
	path := "/vbds?fields=" + url.QueryEscape("VDI,is_cd_drive") + "&filter=" + url.QueryEscape("VM:"+id)
	if err := c.do(ctx, http.MethodGet, path, nil, &vbds); err != nil {
		return nil, 0, err
	}
	for _, vbd := range vbds {
		switch {
		case vbd.VDI == "":
		case vbd.CD:
			cds++
		default:
			var vdi object
			if err := c.do(ctx, http.MethodGet, "/vdis/"+url.PathEscape(vbd.VDI), nil, &vdi); err != nil {
				return nil, 0, err
			}
			disks = append(disks, vdi.NameLabel)
		}
	}
	return disks, cds, nil
}

// TemplateTags returns the tags of a template.
func TemplateTags(ctx context.Context, c *Client, id string) ([]string, error) {
	var o object
	err := c.do(ctx, http.MethodGet, "/vm-templates/"+url.PathEscape(id)+"?fields=tags", nil, &o)
	return o.Tags, err
}
