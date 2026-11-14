//go:build !unix

package tui

func onResize(func()) (stop func()) { return func() {} }
