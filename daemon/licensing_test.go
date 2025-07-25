package daemon

import (
	"testing"

	"github.com/docker/docker/dockerversion"
	"github.com/moby/moby/api/types/system"
	"gotest.tools/v3/assert"
)

func TestFillLicense(t *testing.T) {
	v := &system.Info{}
	d := &Daemon{
		root: "/var/lib/docker/",
	}
	d.fillLicense(v)
	assert.Assert(t, v.ProductLicense == dockerversion.DefaultProductLicense)
}
