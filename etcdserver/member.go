// Copyright 2015 CoreOS, Inc.
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

package etcdserver

import (
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"path"
	"sort"
	"time"

	"github.com/coreos/etcd/pkg/types"
	"github.com/coreos/etcd/store"
	"github.com/coreos/etcd/version"
)

// RaftAttributes represents the raft related attributes of an etcd member.
type RaftAttributes struct {
	// TODO(philips): ensure these are URLs
	PeerURLs []string `json:"peerURLs"`
}

// Attributes represents all the non-raft related attributes of an etcd member.
type Attributes struct {
	Name       string   `json:"name,omitempty"`
	ClientURLs []string `json:"clientURLs,omitempty"`
}

type Member struct {
	ID types.ID `json:"id"`
	RaftAttributes
	Attributes
}

// NewMember creates a Member without an ID and generates one based on the
// name, peer URLs. This is used for bootstrapping/adding new member.
func NewMember(name string, peerURLs types.URLs, clusterName string, now *time.Time) *Member {
	m := &Member{
		RaftAttributes: RaftAttributes{PeerURLs: peerURLs.StringSlice()},
		Attributes:     Attributes{Name: name},
	}

	var b []byte
	sort.Strings(m.PeerURLs)
	for _, p := range m.PeerURLs {
		b = append(b, []byte(p)...)
	}

	b = append(b, []byte(clusterName)...)
	if now != nil {
		b = append(b, []byte(fmt.Sprintf("%d", now.Unix()))...)
	}

	hash := sha1.Sum(b)
	m.ID = types.ID(binary.BigEndian.Uint64(hash[:8]))
	return m
}

// PickPeerURL chooses a random address from a given Member's PeerURLs.
// It will panic if there is no PeerURLs available in Member.
func (m *Member) PickPeerURL() string {
	if len(m.PeerURLs) == 0 {
		log.Panicf("member should always have some peer url")
	}
	return m.PeerURLs[rand.Intn(len(m.PeerURLs))]
}

func (m *Member) Clone() *Member {
	if m == nil {
		return nil
	}
	mm := &Member{
		ID: m.ID,
		Attributes: Attributes{
			Name: m.Name,
		},
	}
	if m.PeerURLs != nil {
		mm.PeerURLs = make([]string, len(m.PeerURLs))
		copy(mm.PeerURLs, m.PeerURLs)
	}
	if m.ClientURLs != nil {
		mm.ClientURLs = make([]string, len(m.ClientURLs))
		copy(mm.ClientURLs, m.ClientURLs)
	}
	return mm
}

func memberStoreKey(id types.ID) string {
	return path.Join(storeMembersPrefix, id.String())
}

func MemberAttributesStorePath(id types.ID) string {
	return path.Join(memberStoreKey(id), attributesSuffix)
}

func mustParseMemberIDFromKey(key string) types.ID {
	id, err := types.IDFromString(path.Base(key))
	if err != nil {
		log.Panicf("unexpected parse member id error: %v", err)
	}
	return id
}

func removedMemberStoreKey(id types.ID) string {
	return path.Join(storeRemovedMembersPrefix, id.String())
}

// nodeToMember builds member from a key value node.
// the child nodes of the given node MUST be sorted by key.
func nodeToMember(n *store.NodeExtern) (*Member, error) {
	m := &Member{ID: mustParseMemberIDFromKey(n.Key)}
	attrs := make(map[string][]byte)
	raftAttrKey := path.Join(n.Key, raftAttributesSuffix)
	attrKey := path.Join(n.Key, attributesSuffix)
	for _, nn := range n.Nodes {
		if nn.Key != raftAttrKey && nn.Key != attrKey {
			return nil, fmt.Errorf("unknown key %q", nn.Key)
		}
		attrs[nn.Key] = []byte(*nn.Value)
	}
	if data := attrs[raftAttrKey]; data != nil {
		if err := json.Unmarshal(data, &m.RaftAttributes); err != nil {
			return nil, fmt.Errorf("unmarshal raftAttributes error: %v", err)
		}
	} else {
		return nil, fmt.Errorf("raftAttributes key doesn't exist")
	}
	if data := attrs[attrKey]; data != nil {
		if err := json.Unmarshal(data, &m.Attributes); err != nil {
			return m, fmt.Errorf("unmarshal attributes error: %v", err)
		}
	}
	return m, nil
}

// getVersion returns the version of the given member via its
// peerURLs. Returns the last error if it fails to get the version.
func getVersion(m *Member, tr *http.Transport) (string, error) {
	cc := &http.Client{
		Transport: tr,
		Timeout:   time.Second,
	}
	var (
		err  error
		resp *http.Response
	)

	for _, u := range m.PeerURLs {
		resp, err = cc.Get(u + "/version")
		if err != nil {
			continue
		}
		b, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		var vers version.Versions
		if err := json.Unmarshal(b, &vers); err != nil {
			continue
		}
		return vers.Server, nil
	}
	return "", err
}

// implement sort by ID interface
type SortableMemberSlice []*Member

func (s SortableMemberSlice) Len() int           { return len(s) }
func (s SortableMemberSlice) Less(i, j int) bool { return s[i].ID < s[j].ID }
func (s SortableMemberSlice) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// implement sort by peer urls interface
type SortableMemberSliceByPeerURLs []*Member

func (p SortableMemberSliceByPeerURLs) Len() int { return len(p) }
func (p SortableMemberSliceByPeerURLs) Less(i, j int) bool {
	return p[i].PeerURLs[0] < p[j].PeerURLs[0]
}
func (p SortableMemberSliceByPeerURLs) Swap(i, j int) { p[i], p[j] = p[j], p[i] }
