// Copyright 2019 Altinity Ltd and/or its affiliates. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"testing"

	"github.com/stretchr/testify/require"

	chi "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	"github.com/altinity/clickhouse-operator/pkg/apis/swversion"
)

// coordHost builds a host whose runtime Keeper version is derived from the
// given image tag (nil tag => unknown version), so getCoordinationSettings can
// be exercised across Keeper versions without a live cluster.
func coordHost(tag string) *chi.Host {
	h := &chi.Host{}
	if tag != "" {
		h.Runtime.Version = swversion.NewSoftWareVersionFromTag(tag)
	}
	return h
}

func coordXML(t *testing.T, tags ...string) string {
	t.Helper()
	hosts := make([]*chi.Host, 0, len(tags))
	for _, tag := range tags {
		hosts = append(hosts, coordHost(tag))
	}
	g := NewGenerator(&fakeCR{hosts: hosts}, &fakeNamer{}, &GeneratorOptions{})
	return g.getCoordinationSettings()
}

func TestCoordinationSettingsModernKeeperEmitsBoth(t *testing.T) {
	for _, tag := range []string{"25.8", "24.12", "latest"} {
		xml := coordXML(t, tag)
		require.Contains(t, xml, "<async_replication>1</async_replication>", "tag %s", tag)
		require.Contains(t, xml, "<use_xid_64>1</use_xid_64>", "tag %s", tag)
	}
}

func TestCoordinationSettings248EmitsAsyncOnly(t *testing.T) {
	// 24.8 supports async_replication but not use_xid_64.
	xml := coordXML(t, "24.8")
	require.Contains(t, xml, "<async_replication>1</async_replication>")
	require.NotContains(t, xml, "use_xid_64")
}

func TestCoordinationSettings238EmitsNothing(t *testing.T) {
	// 23.8 supports neither — nothing must be emitted (empty overlay).
	xml := coordXML(t, "23.8")
	require.NotContains(t, xml, "async_replication")
	require.NotContains(t, xml, "use_xid_64")
	require.Empty(t, xml)
}

func TestCoordinationSettingsUnknownVersionEmitsBoth(t *testing.T) {
	// An unparseable/absent tag must preserve the prior always-on behavior.
	xml := coordXML(t, "")
	require.Contains(t, xml, "<async_replication>1</async_replication>")
	require.Contains(t, xml, "<use_xid_64>1</use_xid_64>")
}

func TestCoordinationSettingsMinVersionSentinelEmitsBoth(t *testing.T) {
	// getHostSoftwareVersion returns swversion.MinVersion() (0.0.1) — not nil —
	// for any unparseable image tag (head, digest-pinned, bare major, custom
	// names, etc). This is the actual production shape of an "unknown" host
	// version, unlike the nil used by the "unknown" test above. The sentinel
	// must be treated as unresolved, not as a real too-old version, so both
	// gated settings are still emitted.
	h := &chi.Host{}
	h.Runtime.Version = swversion.MinVersion()
	g := NewGenerator(&fakeCR{hosts: []*chi.Host{h}}, &fakeNamer{}, &GeneratorOptions{})
	xml := g.getCoordinationSettings()
	require.Contains(t, xml, "<async_replication>1</async_replication>")
	require.Contains(t, xml, "<use_xid_64>1</use_xid_64>")
}

func TestCoordinationSettingsMixedVersionsGatesByOldest(t *testing.T) {
	// The oldest known host governs the shared common config: a 24.8 host
	// alongside a 25.8 host must still omit use_xid_64.
	xml := coordXML(t, "25.8", "24.8")
	require.Contains(t, xml, "<async_replication>1</async_replication>")
	require.NotContains(t, xml, "use_xid_64")
}
