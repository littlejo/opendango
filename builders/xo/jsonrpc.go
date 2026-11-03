package xo

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/coder/websocket"
)

// convertToTemplate calls vm.convertToTemplate on the JSON-RPC API. The
// REST API has no equivalent yet; switch to it once it does.
func (c *Client) convertToTemplate(ctx context.Context, vmID string) error {
	return c.rpc(ctx, "vm.convertToTemplate", map[string]string{"id": vmID})
}

// setBootOrder calls vm.setBootOrder on the JSON-RPC API (HVM boot order:
// c disk, d CD, n network). The REST API cannot change it.
func (c *Client) setBootOrder(ctx context.Context, vmID, order string) error {
	return c.rpc(ctx, "vm.setBootOrder", map[string]string{"vm": vmID, "order": order})
}

// ejectCD calls vm.ejectCd on the JSON-RPC API: the REST API cannot eject.
func (c *Client) ejectCD(ctx context.Context, vmID string) error {
	return c.rpc(ctx, "vm.ejectCd", map[string]string{"id": vmID})
}

// rpc signs in on the JSON-RPC API (WebSocket on /api/) and calls method.
func (c *Client) rpc(ctx context.Context, method string, params any) error {
	wsURL := "ws" + strings.TrimPrefix(c.BaseURL, "http") + "/api/"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: c.HTTP})
	if err != nil {
		return fmt.Errorf("xo: JSON-RPC: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 20)

	rpc := &jsonRPC{conn: conn}
	if err := rpc.call(ctx, "session.signIn", map[string]string{"token": c.Token}); err != nil {
		return err
	}
	return rpc.call(ctx, method, params)
}

type jsonRPC struct {
	conn   *websocket.Conn
	nextID int
}

func (r *jsonRPC) call(ctx context.Context, method string, params any) error {
	r.nextID++
	req, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": r.nextID, "method": method, "params": params})
	if err != nil {
		return err
	}
	if err := r.conn.Write(ctx, websocket.MessageText, req); err != nil {
		return fmt.Errorf("xo: JSON-RPC %s: %w", method, err)
	}
	for {
		_, data, err := r.conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("xo: JSON-RPC %s: %w", method, err)
		}
		var resp struct {
			ID    *int `json:"id"`
			Error *struct {
				Message string          `json:"message"`
				Data    json.RawMessage `json:"data"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return fmt.Errorf("xo: JSON-RPC %s: %w", method, err)
		}
		// Skip notifications (no id) and stale responses.
		if resp.ID == nil || *resp.ID != r.nextID {
			continue
		}
		if resp.Error != nil {
			return fmt.Errorf("xo: JSON-RPC %s: %s %s", method, resp.Error.Message, string(resp.Error.Data))
		}
		return nil
	}
}
