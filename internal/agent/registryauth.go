package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/prezmc/tainerhub/internal/regcred"
	"github.com/prezmc/tainerhub/internal/wire"
)

// helperAuthDir is where the self-update helper finds its registry logins
// (its DOCKER_CONFIG), relative to its root.
const helperAuthDir = "tainerhub-auth"

// authArchive is a tar holding helperAuthDir/config.json with just these
// logins, readable only by root.
func authArchive(auths []wire.RegistryAuth) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	now := time.Now()
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: helperAuthDir + "/", Mode: 0o700, ModTime: now})
	cfg := regcred.DockerConfigJSON(auths)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: helperAuthDir + "/config.json", Mode: 0o600, Size: int64(len(cfg)), ModTime: now})
	_, _ = tw.Write(cfg)
	_ = tw.Close()
	return buf.Bytes()
}

// putArchive extracts a tar into a (created, not yet started) container.
func (d *SocketDocker) putArchive(ctx context.Context, id, path string, archive []byte) error {
	u := "http://docker/containers/" + url.PathEscape(id) + "/archive?" + url.Values{"path": {path}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(archive))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := d.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("docker: %s", resp.Status)
	}
	return nil
}

// registryLogin asks the Docker Engine to check a login with its registry
// (POST /auth). Docker doesn't save it: the next pull still needs it sent.
func (d *SocketDocker) registryLogin(ctx context.Context, a wire.RegistryAuth) (wire.RegistryLoginResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body := map[string]string{"username": a.Username, "password": a.Password, "serveraddress": regcred.ServerAddress(a.Registry)}
	resp, err := d.doBody(ctx, http.MethodPost, "/auth", nil, body)
	if err != nil {
		return wire.RegistryLoginResult{}, err
	}
	var out struct{ Status string }
	_ = jsonDecode(resp, &out)
	if out.Status == "" {
		out.Status = "Login Succeeded"
	}
	return wire.RegistryLoginResult{Status: out.Status}, nil
}
