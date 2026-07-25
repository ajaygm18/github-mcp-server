package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/github/github-mcp-server/pkg/github"
	"github.com/github/github-mcp-server/pkg/inventory"
	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024 * 64,
	WriteBufferSize: 1024 * 64,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for MCP clients
	},
}

type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Result  any    `json:"result,omitempty"`
	Error   any    `json:"error,omitempty"`
}

func (h *Handler) ServeWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Error("failed to upgrade to websocket", "error", err)
		return
	}
	defer conn.Close()

	h.logger.Info("new websocket client connected", "remoteAddr", r.RemoteAddr)

	var mu sync.Mutex
	// Set up 20-second ping ticker to keep Heroku HTTP router connection alive indefinitely
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go func() {
		for {
			select {
			case <-ticker.C:
				mu.Lock()
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
					mu.Unlock()
					cancel()
					return
				}
				mu.Unlock()
			case <-ctx.Done():
				return
			}
		}
	}()

	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				h.logger.Error("websocket closed unexpectedly", "error", err)
			}
			break
		}

		if messageType != websocket.TextMessage {
			continue
		}

		go func(rawPayload []byte) {
			respBytes := h.dispatchWSJSONRPC(r, rawPayload)
			if len(respBytes) > 0 {
				mu.Lock()
				conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
				_ = conn.WriteMessage(websocket.TextMessage, respBytes)
				mu.Unlock()
			}
		}(payload)
	}
}

func (h *Handler) dispatchWSJSONRPC(r *http.Request, payload []byte) []byte {
	var req JSONRPCRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return formatJSONRPCError(nil, -32700, "Parse error: invalid JSON")
	}

	toolCtx := r.Context()
	inv, err := h.inventoryFactoryFunc(r)
	if err != nil {
		return formatJSONRPCError(req.ID, -32603, fmt.Sprintf("Inventory error: %v", err))
	}

	switch req.Method {
	case "initialize":
		res := map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "github-mcp-server-websocket",
				"version": h.config.Version,
			},
		}
		return formatJSONRPCResult(req.ID, res)

	case "ping":
		return formatJSONRPCResult(req.ID, map[string]any{})

	case "tools/list":
		tools := inv.AvailableTools(toolCtx)
		mcpTools := make([]map[string]any, 0, len(tools))
		for _, st := range tools {
			toolMap := map[string]any{
				"name":        st.Tool.Name,
				"description": st.Tool.Description,
				"inputSchema": st.Tool.InputSchema,
			}
			mcpTools = append(mcpTools, toolMap)
		}
		return formatJSONRPCResult(req.ID, map[string]any{"tools": mcpTools})

	case "tools/call":
		var callParams struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &callParams); err != nil {
			return formatJSONRPCError(req.ID, -32602, "Invalid params for tools/call")
		}

		tools := inv.AvailableTools(toolCtx)
		var targetTool *inventory.ServerTool
		for _, st := range tools {
			if st.Tool.Name == callParams.Name {
				stCopy := st
				targetTool = &stCopy
				break
			}
		}

		if targetTool == nil {
			return formatJSONRPCError(req.ID, -32601, fmt.Sprintf("Tool '%s' not found", callParams.Name))
		}

		toolCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		toolCtx = github.ContextWithDeps(toolCtx, h.deps)

		argsBytes, _ := json.Marshal(callParams.Arguments)
		callReq := &mcp.CallToolRequest{}
		callReq.Params = &mcp.CallToolParamsRaw{
			Name:      callParams.Name,
			Arguments: argsBytes,
		}

		// Instantiate tool handler with request dependencies
		handler := targetTool.Handler(h.deps)
		res, err := handler(toolCtx, callReq)
		if err != nil {
			return formatJSONRPCError(req.ID, -32603, fmt.Sprintf("Tool execution error: %v", err))
		}

		return formatJSONRPCResult(req.ID, res)

	default:
		return formatJSONRPCError(req.ID, -32601, fmt.Sprintf("Method '%s' not supported over WebSocket", req.Method))
	}
}

func formatJSONRPCResult(id any, result any) []byte {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	b, _ := json.Marshal(resp)
	return b
}

func formatJSONRPCError(id any, code int, message string) []byte {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: map[string]any{
			"code":    code,
			"message": message,
		},
	}
	b, _ := json.Marshal(resp)
	return b
}
