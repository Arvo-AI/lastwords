//go:build !linux

package jvm

import (
	"context"
	"errors"
	"net"
	"os"
)

type handle struct{}

var errUnsupported = errors.New("JVM attach is only supported on Linux")

// New is only implemented on Linux, where the agent runs.
func New(int) (*Client, error) { return nil, errUnsupported }

// Close does nothing outside Linux.
func (c *Client) Close() error { c.h = handle{}; return nil }

func (c *Client) ensureListener(context.Context) error   { return errUnsupported }
func (c *Client) dial(context.Context) (net.Conn, error) { return nil, errUnsupported }
func (c *Client) OpenTmpFile(string) (*os.File, error)   { return nil, errUnsupported }
func (c *Client) RemoveTmpFile(string)                   {}
