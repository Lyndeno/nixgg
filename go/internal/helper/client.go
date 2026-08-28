package helper

import (
	"bufio"
	"fmt"
	"net"
)

// Client is a one-shot connection to a running helper's socket —
// matches the real usage pattern (one shim invocation makes at most
// two calls total: one StoreAddScan, one DerivationAdd, each its own
// short-lived process). Not pooled client-side; the helper's own Pool
// is where connection reuse happens.

// AddDerivation relays a DerivationAdd call through the helper at
// socketPath. Same signature/contract as internal/rpc.Conn's own
// method.
func AddDerivation(socketPath, name string, contents []byte, refs []string) (string, error) {
	resp, err := roundTrip(socketPath, request{
		Op:       "AddDerivation",
		Name:     name,
		Contents: contents,
		Refs:     refs,
	})
	if err != nil {
		return "", err
	}
	if resp.Err != "" {
		return "", fmt.Errorf("%s", resp.Err)
	}
	return resp.StorePath, nil
}

// AddToStoreScanning relays an AddToStoreScanning call through the
// helper at socketPath.
func AddToStoreScanning(socketPath, name string, narDump []byte) (string, error) {
	resp, err := roundTrip(socketPath, request{
		Op:      "AddToStoreScanning",
		Name:    name,
		NarDump: narDump,
	})
	if err != nil {
		return "", err
	}
	if resp.Err != "" {
		return "", fmt.Errorf("%s", resp.Err)
	}
	return resp.StorePath, nil
}

// SubmitOutput relays a SubmitOutput call through the helper at
// socketPath.
func SubmitOutput(socketPath, drvPath, output string) error {
	resp, err := roundTrip(socketPath, request{
		Op:      "SubmitOutput",
		DrvPath: drvPath,
		Output:  output,
	})
	if err != nil {
		return err
	}
	if resp.Err != "" {
		return fmt.Errorf("%s", resp.Err)
	}
	return nil
}

func roundTrip(socketPath string, req request) (response, error) {
	c, err := net.Dial("unix", socketPath)
	if err != nil {
		return response{}, fmt.Errorf("helper: dial %s: %w", socketPath, err)
	}
	defer c.Close()

	w := bufio.NewWriter(c)
	if err := writeFrame(w, &req); err != nil {
		return response{}, err
	}

	var resp response
	if err := readFrame(bufio.NewReader(c), &resp); err != nil {
		return response{}, err
	}
	return resp, nil
}
