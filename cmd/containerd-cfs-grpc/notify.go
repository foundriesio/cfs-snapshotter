// Copyright (c) Qualcomm Technologies, Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"os"
	"strings"

	"github.com/containerd/log"
)

// notify sends one message on the systemd notification socket named by
// NOTIFY_SOCKET: readiness for the unit's Type=notify startup, and STATUS
// lines that systemctl status shows, so a probe finding is visible where the
// operator looks first. Outside systemd the variable is unset and notify is a
// no-op, so the daemon runs unchanged from a shell. An "@"-prefixed name is
// an abstract-namespace socket.
func notify(ctx context.Context, state string) {
	name := os.Getenv("NOTIFY_SOCKET")
	if name == "" {
		return
	}
	if strings.HasPrefix(name, "@") {
		name = "\x00" + name[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		log.G(ctx).WithError(err).Warn("systemd notify: dial")
		return
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.G(ctx).WithError(err).Warn("systemd notify: close")
		}
	}()
	if _, err := conn.Write([]byte(state)); err != nil {
		log.G(ctx).WithError(err).Warn("systemd notify: write")
	}
}
