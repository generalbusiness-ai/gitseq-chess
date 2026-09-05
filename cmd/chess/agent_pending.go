package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxAgentPending = 32 << 10

type agentCustody struct {
	key  ed25519.PrivateKey
	root *os.Root
	lock *os.File
}

func openAgentCustody(ctx context.Context, path string) (*agentCustody, error) {
	store, err := openKeyStore(ctx, path, "", true)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	directory, err := store.root.Stat(".")
	if err != nil || directory.Mode().Perm()&0022 != 0 {
		return nil, errors.New("agent key directory must not be writable by other users")
	}
	named, err := store.root.Lstat(store.name)
	if err != nil {
		return nil, fmt.Errorf("read existing agent key: %w", err)
	}
	if err = privateAgentFile(named); err != nil {
		return nil, err
	}
	file, err := openAgentFile(store.root, store.name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	if err = checkAgentNamedFile(store.root, store.name, file); err != nil {
		file.Close()
		return nil, err
	}
	encoded, err := io.ReadAll(io.LimitReader(file, maxSecretBytes+1))
	file.Close()
	if err != nil {
		return nil, err
	}
	if len(encoded) > maxSecretBytes {
		return nil, errors.New("agent key file is oversized")
	}
	key, err := decodeKey(encoded)
	if err != nil {
		return nil, err
	}
	current, err := store.root.Lstat(store.name)
	if err != nil || !os.SameFile(named, current) {
		return nil, errors.New("agent key was replaced")
	}
	root, err := openDirectory(store.root, "."+store.name+".agent")
	if err != nil {
		return nil, err
	}
	c := &agentCustody{key: key, root: root}
	fail := func(err error) (*agentCustody, error) { c.Close(); return nil, err }
	lock, err := openAgentFile(root, "lock", os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return fail(err)
	}
	c.lock = lock
	if err = checkAgentNamedFile(root, "lock", lock); err != nil {
		return fail(err)
	}
	if err = lockAgentFile(lock); err != nil {
		return fail(err)
	}
	if err = checkAgentNamedFile(root, "lock", lock); err != nil {
		return fail(err)
	}
	// The first explicit use pins the public identity. Later key replacement,
	// even between CLI processes, cannot silently claim an existing seat.
	public := key.Public().(ed25519.PublicKey)
	pinned, err := c.read("identity")
	if errors.Is(err, os.ErrNotExist) {
		err = c.publish("identity", public)
	} else if err == nil && !bytes.Equal(pinned, public) {
		err = errors.New("agent key was replaced; restore the original key")
	}
	if err != nil {
		return fail(err)
	}
	return c, nil
}

func checkAgentNamedFile(root *os.Root, name string, file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if err = privateAgentFile(info); err != nil {
		return err
	}
	named, err := root.Lstat(name)
	if err != nil || !os.SameFile(info, named) || !named.Mode().IsRegular() {
		return errors.New("agent custody file was replaced")
	}
	return nil
}

func (c *agentCustody) Close() {
	if c.lock != nil {
		c.lock.Close()
	}
	if c.root != nil {
		c.root.Close()
	}
}
func (c *agentCustody) read(name string) ([]byte, error) {
	file, err := openAgentFile(c.root, name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if err = checkAgentNamedFile(c.root, name, file); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxAgentPending+1))
	if err == nil && len(data) > maxAgentPending {
		err = errors.New("agent custody file is oversized")
	}
	return data, err
}
func (c *agentCustody) sync() error {
	dir, err := c.root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Publication is atomic and exclusive, never an overwrite of an unknown act.
func (c *agentCustody) publish(name string, data []byte) error {
	if len(data) > maxAgentPending {
		return errors.New("pending agent action is oversized")
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	temp := "." + hex.EncodeToString(token) + ".tmp"
	file, err := c.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer c.root.Remove(temp)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = c.root.Link(temp, name); err != nil {
		return err
	}
	// Remove the temporary link before another read applies the single-link rule.
	if err = c.root.Remove(temp); err != nil {
		return err
	}
	return c.sync()
}
func (c *agentCustody) pending() (*agentSubmission, error) {
	data, err := c.read("pending.json")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pending agentSubmission
	if err = strictJSON(data, &pending); err != nil {
		return nil, errors.New("retained agent action is malformed; preserve it for recovery")
	}
	if _, err = pending.Action.act(); err != nil {
		return nil, err
	}
	if !bytes.Equal(pending.Action.ActorKey, c.key.Public().(ed25519.PublicKey)) || len(pending.Signature) != ed25519.SignatureSize {
		return nil, errors.New("retained agent action does not match this key")
	}
	return &pending, nil
}
func (c *agentCustody) retain(value agentSubmission) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.publish("pending.json", data)
}
func (c *agentCustody) clear() error {
	if err := c.root.Remove("pending.json"); err != nil {
		return err
	}
	return c.sync()
}

func runAgentKey(ctx context.Context, args []string, stdout io.Writer) error {
	// Key creation has its own command so service actions never create a key.
	set := newAgentFlagSet("keygen")
	path := set.String("key", "", "new private-key file")
	if err := parseNoPositionals(set, args); err != nil {
		return err
	}
	if *path == "" || filepath.Base(*path) == "." {
		return errors.New("keygen requires --key")
	}
	store, err := openKeyStore(ctx, *path, "", true)
	if err != nil {
		return err
	}
	defer store.Close()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err = publishKey(store, key); err != nil {
		return err
	}
	return writeJSON(stdout, map[string]string{"key": *path, "actor": liveFingerprint(key.Public().(ed25519.PublicKey))})
}
