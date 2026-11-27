package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/prezmc/tainerhub/internal/wire"
)

// createShareVolume creates a volume that mounts an SMB/CIFS share or an
// NFS export on a host (Resources → Volumes → New network share). The
// agent checks it mounts and removes it again if not.
//
// The share's password goes to the agent for this one operation and into
// the volume's options on the host (where Docker keeps it in plain text;
// see docs/security.md). The hub doesn't store it, and the audit log and
// errors never contain it.
func (s *Server) createShareVolume(w http.ResponseWriter, r *http.Request) {
	var in wire.ShareVolume
	if !readJSON(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Server = strings.TrimSpace(in.Server)
	in.Path = strings.TrimSpace(in.Path)
	in.Username = strings.TrimSpace(in.Username)
	in.Domain = strings.TrimSpace(in.Domain)
	if err := wire.ValidShareVolume(&in); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a := s.Reg.Get(r.PathValue("id"))
	if a == nil {
		jsonErr(w, http.StatusServiceUnavailable, "host is offline")
		return
	}
	data, err := a.Operate(r.Context(), wire.Operation{Kind: wire.OpVolumeCreateShare, Share: &in})
	detail := shareAuditDetail(a.Name, &in, err)
	u := userFrom(r)
	s.record(r, &u, wire.OpVolumeCreateShare, a.Name+"/"+in.Name, detail)
	if err != nil {
		jsonErr(w, http.StatusBadGateway, hidePassword(err.Error(), in.Password))
		return
	}
	var res wire.ShareCreated
	if err := json.Unmarshal(data, &res); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// shareAuditDetail describes a share volume for the audit log: where it
// points and who it logs in as, never the password.
func shareAuditDetail(host string, in *wire.ShareVolume, err error) map[string]any {
	d := map[string]any{"host": host, "type": in.Type, "server": in.Server, "path": in.Path}
	if in.Username != "" {
		d["username"] = in.Username
		if in.Password != "" {
			d["password"] = hiddenValue
		}
	}
	for k, v := range map[string]string{"domain": in.Domain, "version": in.Version, "uid": in.UID, "gid": in.GID, "file_mode": in.FileMode, "dir_mode": in.DirMode} {
		if v != "" {
			d[k] = v
		}
	}
	if in.ReadOnly {
		d["read_only"] = true
	}
	if err != nil {
		d["error"] = hidePassword(err.Error(), in.Password)
	}
	return d
}

// hidePassword blanks a password wherever it appears in msg. The agent
// already leaves it out of its errors; this is the hub's own guard.
func hidePassword(msg, pw string) string {
	if pw == "" {
		return msg
	}
	return strings.ReplaceAll(msg, pw, hiddenValue)
}

// installShareTools installs the host's package for mounting SMB (cifs-utils)
// or NFS (nfs-common / nfs-utils) shares. Body: {"type": "cifs"|"nfs"}.
func (s *Server) installShareTools(w http.ResponseWriter, r *http.Request) {
	var in struct{ Type string }
	if !readJSON(w, r, &in) {
		return
	}
	s.runOp(w, r, wire.Operation{Kind: wire.OpHostInstallShareTools, Arg: in.Type}, "share tools")
}
