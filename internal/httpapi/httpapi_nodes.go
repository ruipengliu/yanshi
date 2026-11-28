package httpapi

import (
	"net/http"

	"yanshi/internal/node"
)

type nodeView struct {
	NodeID       string   `json:"node_id"`
	Label        string   `json:"label"`
	Kind         string   `json:"kind"`
	HostApp      string   `json:"host_app"`
	Online       bool     `json:"online"`
	Capabilities []string `json:"capabilities"`
}

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	if s.Nodes == nil {
		writeJSON(w, http.StatusOK, map[string]any{"nodes": []nodeView{}})
		return
	}
	q := r.URL.Query()
	bl, eu, err := claim(principal(r), q.Get("business_line"), q.Get("end_user"))
	if err != nil {
		s.fail(w, err)
		return
	}
	nodes, err := s.Nodes.List(r.Context(), node.Scope{BusinessLine: bl, EndUser: eu})
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []nodeView{}
	for _, n := range nodes {
		v := nodeView{NodeID: n.NodeID, Label: n.Label, Kind: n.Kind, HostApp: n.HostApp, Online: n.Online, Capabilities: []string{}}
		for _, c := range n.Capabilities {
			v.Capabilities = append(v.Capabilities, c.GetName())
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": out})
}
