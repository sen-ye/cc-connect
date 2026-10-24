// Package acp registers the generic Agent Client Protocol adapter.
package acp

import (
	"github.com/chenhg5/cc-connect/core"
	"github.com/chenhg5/cc-connect/internal/acpclient"
)

type Agent = acpclient.Agent

func init() { core.RegisterAgent("acp", New) }

func New(opts map[string]any) (core.Agent, error) { return acpclient.New(opts) }
