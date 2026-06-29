package tpu

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	// Solana TPU QUIC ALPN (agave streamer).
	alpnSolanaTPU = "solana-tpu"
	probeTimeout  = 2 * time.Second
)

// Probe checks whether the active validator TPU is reachable.
// Prefers tpuQuic (QUIC handshake); falls back to legacy tpu (UDP).
func Probe(tpuQuic, tpu string) bool {
	if tpuQuic != "" {
		return probeQUIC(tpuQuic)
	}
	if tpu != "" {
		return probeUDP(tpu)
	}
	return false
}

// ProbeLabel returns a human-readable probe target for logs.
func ProbeLabel(tpuQuic, tpu string) string {
	if tpuQuic != "" {
		return "TPU QUIC " + tpuQuic
	}
	if tpu != "" {
		return "TPU UDP " + tpu
	}
	return "TPU (none configured)"
}

func probeQUIC(address string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{alpnSolanaTPU},
		MinVersion:         tls.VersionTLS13,
	}

	conn, err := quic.DialAddr(ctx, address, tlsConf, &quic.Config{
		HandshakeIdleTimeout: probeTimeout,
		MaxIdleTimeout:       probeTimeout,
	})
	if err != nil {
		return false
	}
	_ = conn.CloseWithError(0, "")
	return true
}

func probeUDP(address string) bool {
	conn, err := net.DialTimeout("udp", address, probeTimeout)
	if err != nil {
		return false
	}
	defer conn.Close()

	deadline := time.Now().Add(probeTimeout)
	if err := conn.SetDeadline(deadline); err != nil {
		return false
	}

	// Connected UDP: ICMP port unreachable surfaces as ECONNREFUSED on write/read.
	if _, err := conn.Write([]byte{0}); err != nil {
		return !isPortClosed(err)
	}

	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	if err == nil {
		return true
	}
	if isPortClosed(err) {
		return false
	}
	// Timeout with no ICMP unreachable — port likely open (or silently dropped).
	return true
}

func isPortClosed(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return errors.Is(opErr.Err, syscall.ECONNREFUSED)
	}
	return false
}
