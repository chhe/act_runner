// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package client

import "gitea.dev/actionslib/pkg/protocol"

// The headers are defined in the shared protocol package so that Gitea and the
// runner cannot drift apart.
const (
	UUIDHeader  = protocol.UUIDHeader
	TokenHeader = protocol.TokenHeader
)
