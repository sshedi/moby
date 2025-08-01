package container

import (
	"context"
	"os"
	"testing"

	"github.com/moby/moby/v2/testutil"
	"github.com/moby/moby/v2/testutil/environment"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

var (
	testEnv     *environment.Execution
	baseContext context.Context
)

func TestMain(m *testing.M) {
	var err error
	shutdown := testutil.ConfigureTracing()

	ctx, span := otel.Tracer("").Start(context.Background(), "integration/container/TestMain")
	baseContext = ctx

	testEnv, err = environment.New(ctx)
	if err != nil {
		span.SetStatus(codes.Error, "environment.New failed")
		span.End()
		shutdown(ctx)
		panic(err)
	}
	err = environment.EnsureFrozenImagesLinux(ctx, testEnv)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.End()
		shutdown(ctx)
		panic(err)
	}

	testEnv.Print()
	code := m.Run()
	if code != 0 {
		span.SetStatus(codes.Error, "m.Run() exited with non-zero code")
	}
	span.End()
	shutdown(ctx)
	os.Exit(code)
}

func setupTest(t *testing.T) context.Context {
	ctx := testutil.StartSpan(baseContext, t)
	environment.ProtectAll(ctx, t, testEnv)
	t.Cleanup(func() { testEnv.Clean(ctx, t) })
	return ctx
}
