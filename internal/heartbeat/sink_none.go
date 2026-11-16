package heartbeat

import "context"

type NoopSink struct{}

func (NoopSink) Write(context.Context, Record) error { return nil }

func (NoopSink) List(context.Context, string, int) ([]Record, error) { return nil, nil }

func (NoopSink) Close() error { return nil }
