package konnect_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/google/uuid"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kong/kubernetes-ingress-controller/v2/internal/dataplane"
	"github.com/kong/kubernetes-ingress-controller/v2/internal/konnect"
)

const (
	testKicVersion  = "2.9.0"
	testKongVersion = "3.2.0.0"
	testClusterID   = "cluster-00"
)

// mockKonnectNodeService provides a mock service for CRUD of konnect nodes.
type mockKonnectNodeService struct {
	lock      sync.RWMutex
	clusterID string
	nodes     []*konnect.NodeItem

	returnErrorFromListNodes bool
	wasListNodesCalled       bool
}

func (s *mockKonnectNodeService) upsertNode(nodeID string, version string, hostname string, lastping int64, typ string, status string) *konnect.NodeItem {
	s.lock.Lock()
	defer s.lock.Unlock()

	for _, node := range s.nodes {
		if node.ID == nodeID {
			node.LastPing = lastping
			node.Status = status
			node.Version = version
			node.UpdatedAt = time.Now().Unix()
			return node
		}
	}

	node := &konnect.NodeItem{
		ID:        uuid.NewString(),
		Version:   version,
		Hostname:  hostname,
		LastPing:  lastping,
		Type:      typ,
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
		Status:    status,
	}
	s.nodes = append(s.nodes, node)
	return node
}

func (s *mockKonnectNodeService) handleCreateNode(rw http.ResponseWriter, body []byte) {
	createNodeReq := &konnect.CreateNodeRequest{}
	err := json.Unmarshal(body, createNodeReq)
	if err != nil {
		rw.WriteHeader(http.StatusBadRequest)
		_, _ = rw.Write([]byte("bad req body"))
		return
	}

	node := s.upsertNode(
		createNodeReq.ID,
		createNodeReq.Version,
		createNodeReq.Hostname,
		createNodeReq.LastPing,
		createNodeReq.Type,
		createNodeReq.Status,
	)
	resp := konnect.CreateNodeResponse{Item: node}
	buf, _ := json.Marshal(resp)
	_, _ = rw.Write(buf)
}

func (s *mockKonnectNodeService) handleUpdateNode(rw http.ResponseWriter, nodeID string, body []byte) {
	updateNodeReq := &konnect.UpdateNodeRequest{}
	err := json.Unmarshal(body, updateNodeReq)
	if err != nil {
		rw.WriteHeader(http.StatusBadRequest)
		_, _ = rw.Write([]byte("bad req body"))
		return
	}

	node := s.upsertNode(
		nodeID,
		updateNodeReq.Version,
		updateNodeReq.Hostname,
		updateNodeReq.LastPing,
		updateNodeReq.Type,
		updateNodeReq.Status,
	)
	resp := konnect.UpdateNodeResponse{Item: node}
	buf, _ := json.Marshal(resp)
	_, _ = rw.Write(buf)
}

func (s *mockKonnectNodeService) handleListNodes(rw http.ResponseWriter) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.wasListNodesCalled = true

	if s.returnErrorFromListNodes {
		rw.WriteHeader(http.StatusInternalServerError)
		_, _ = rw.Write([]byte("cannot list nodes"))
		return
	}

	resp := konnect.ListNodeResponse{
		Items: s.nodes,
		Page: &konnect.PaginationInfo{
			TotalCount: int32(len(s.nodes)),
		},
	}
	buf, _ := json.Marshal(resp)
	_, _ = rw.Write(buf)
}

func (s *mockKonnectNodeService) handleDeleteNode(rw http.ResponseWriter, nodeID string) {
	s.lock.Lock()
	defer s.lock.Unlock()

	found := false
	var deleteIdx int
	for i, node := range s.nodes {
		if node.ID == nodeID {
			found = true
			deleteIdx = i
		}
	}
	if found {
		nodes := []*konnect.NodeItem{}
		if deleteIdx > 0 {
			nodes = s.nodes[0 : deleteIdx-1]
		}
		if deleteIdx < len(s.nodes)-1 {
			nodes = append(nodes, s.nodes[deleteIdx+1:]...)
		}
		s.nodes = nodes
	}
	rw.WriteHeader(http.StatusOK)
}

func (s *mockKonnectNodeService) dumpNodes() []*konnect.NodeItem {
	s.lock.RLock()
	defer s.lock.RUnlock()

	copied := make([]*konnect.NodeItem, len(s.nodes))
	copy(copied, s.nodes)
	return copied
}

func (s *mockKonnectNodeService) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	kicNodeAPIRoot := fmt.Sprintf(konnect.KicNodeAPIPathPattern, "", s.clusterID)
	body, err := io.ReadAll(req.Body)
	if err != nil {
		rw.WriteHeader(http.StatusBadRequest)
		_, _ = rw.Write([]byte(""))
	}
	defer req.Body.Close()

	if req.Method == "POST" && req.URL.Path == kicNodeAPIRoot {
		s.handleCreateNode(rw, body)
		return
	}
	if req.Method == "PUT" && strings.Contains(req.URL.Path, kicNodeAPIRoot+"/") {
		nodeID := strings.TrimPrefix(req.URL.Path, kicNodeAPIRoot+"/")
		s.handleUpdateNode(rw, nodeID, body)
		return
	}
	if req.Method == "GET" && req.URL.Path == kicNodeAPIRoot {
		s.handleListNodes(rw)
		return
	}
	if req.Method == "DELETE" && strings.Contains(req.URL.Path, kicNodeAPIRoot+"/") {
		nodeID := strings.TrimPrefix(req.URL.Path, kicNodeAPIRoot+"/")
		s.handleDeleteNode(rw, nodeID)
		return
	}

	rw.WriteHeader(http.StatusFound)
	_, _ = rw.Write([]byte("Not Found"))
}

func (s *mockKonnectNodeService) WasListNodesCalled() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.wasListNodesCalled
}

type mockGatewayInstanceGetter struct {
	gatewayInstances []konnect.GatewayInstance
}

func (m *mockGatewayInstanceGetter) GetGatewayInstances() ([]konnect.GatewayInstance, error) {
	return m.gatewayInstances, nil
}

type mockGatewayClientsNotifier struct {
	ch chan struct{}
}

func newMockGatewayClientsNotifier() *mockGatewayClientsNotifier {
	return &mockGatewayClientsNotifier{
		ch: make(chan struct{}),
	}
}

func (m *mockGatewayClientsNotifier) SubscribeToGatewayClientsChanges() (<-chan struct{}, bool) {
	return m.ch, true
}

func (m *mockGatewayClientsNotifier) Notify() {
	m.ch <- struct{}{}
}

func TestNodeAgentUpdateNodes(t *testing.T) {
	testCases := []struct {
		name         string
		hostname     string
		initialNodes []*konnect.NodeItem
		// when configStatus is non-nil, notify the status to node agent in the test case.
		configStatus     *dataplane.ConfigStatus
		gatewayInstances []konnect.GatewayInstance
		containNodes     []*konnect.NodeItem
		notContainNodes  []*konnect.NodeItem
		numNodes         int
	}{
		{
			name:     "create kic node",
			hostname: "ingress-0",
			// no existing nodes
			initialNodes: nil,
			configStatus: lo.ToPtr(dataplane.ConfigStatusOK),
			containNodes: []*konnect.NodeItem{
				{
					Hostname: "ingress-0",
					Type:     konnect.NodeTypeIngressController,
					Status:   string(konnect.IngressControllerStateOperational),
					Version:  testKicVersion,
				},
			},
			numNodes: 1,
		},
		{
			name:     "update status existing kic node",
			hostname: "ingress-0",
			initialNodes: []*konnect.NodeItem{
				{
					Hostname: "ingress-0",
					ID:       uuid.NewString(),
					Type:     konnect.NodeTypeIngressController,
					Status:   string(konnect.IngressControllerStateOperational),
					Version:  testKicVersion,
				},
			},
			configStatus: lo.ToPtr(dataplane.ConfigStatusTranslationErrorHappened),
			containNodes: []*konnect.NodeItem{
				{
					Hostname: "ingress-0",
					Type:     konnect.NodeTypeIngressController,
					Status:   string(konnect.IngressControllerStatePartialConfigFail),
					Version:  testKicVersion,
				},
			},
			numNodes: 1,
		},
		{
			name:     "remove outdated KIC nodes",
			hostname: "ingress-0",
			initialNodes: []*konnect.NodeItem{
				// older node with same hostname, should delete this.
				{
					Hostname: "ingress-0",
					ID:       uuid.NewString(),
					Type:     konnect.NodeTypeIngressController,
					Status:   string(konnect.IngressControllerStatePartialConfigFail),
					Version:  testKicVersion,
					LastPing: time.Now().Unix() - 10,
				},
				// newer node, should reserve this.
				{
					Hostname: "ingress-0",
					ID:       uuid.NewString(),
					Type:     konnect.NodeTypeIngressController,
					Status:   string(konnect.IngressControllerStateOperational),
					Version:  testKicVersion,
					LastPing: time.Now().Unix() - 3,
				},
				// KIC node with other name, should delete this.
				{
					Hostname: "ingress-1",
					ID:       uuid.NewString(),
					Type:     konnect.NodeTypeIngressController,
					Status:   string(konnect.IngressControllerStateOperational),
					Version:  testKicVersion,
					LastPing: time.Now().Unix() - 3,
				},
			},
			containNodes: []*konnect.NodeItem{
				{
					Hostname: "ingress-0",

					Type:    konnect.NodeTypeIngressController,
					Status:  string(konnect.IngressControllerStateOperational),
					Version: testKicVersion,
				},
			},
			notContainNodes: []*konnect.NodeItem{
				{
					Hostname: "ingress-1",
					Type:     konnect.NodeTypeIngressController,
					Status:   string(konnect.IngressControllerStateOperational),
					Version:  testKicVersion,
				},
			},
			numNodes: 1,
		},
		{
			name:     "update gateway nodes and remove outdated nodes",
			hostname: "ingress-0",
			initialNodes: []*konnect.NodeItem{
				{
					Hostname: "ingress-0",
					ID:       uuid.NewString(),
					Type:     konnect.NodeTypeIngressController,
					Status:   string(konnect.IngressControllerStateOperational),
					Version:  testKicVersion,
				},
				{
					Hostname: "proxy-0",
					ID:       uuid.NewString(),
					Type:     konnect.NodeTypeKongProxy,
					Version:  testKongVersion,
				},
				// 2 gateway nodes with same name, should reserve newer one.
				{
					Hostname: "proxy-1",
					ID:       uuid.NewString(),
					Type:     konnect.NodeTypeKongProxy,
					Version:  testKongVersion,
					LastPing: time.Now().Unix() - 10,
				},
				{
					Hostname: "proxy-1",
					ID:       uuid.NewString(),
					Type:     konnect.NodeTypeKongProxy,
					Version:  testKongVersion,
					LastPing: time.Now().Unix() - 5,
				},
			},
			gatewayInstances: []konnect.GatewayInstance{
				{Hostname: "proxy-1", Version: testKongVersion},
			},
			containNodes: []*konnect.NodeItem{
				{
					Hostname: "ingress-0",
					Type:     konnect.NodeTypeIngressController,
					Status:   string(konnect.IngressControllerStateOperational),
					Version:  testKicVersion,
				},
				{
					Hostname: "proxy-1",
					Type:     konnect.NodeTypeKongProxy,
					Version:  testKongVersion,
				},
			},
			notContainNodes: []*konnect.NodeItem{
				{
					Hostname: "proxy-0",
					Type:     konnect.NodeTypeKongProxy,
					Version:  testKongVersion,
				},
			},
			numNodes: 2,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			nodeService := &mockKonnectNodeService{
				clusterID: testClusterID,
				nodes:     tc.initialNodes,
			}
			s := httptest.NewServer(nodeService)
			nodeClient := &konnect.NodeAPIClient{
				Address:        s.URL,
				RuntimeGroupID: testClusterID,
				Client:         &http.Client{},
			}

			logger := testr.New(t)
			configStatusSubscriber := dataplane.NewChannelConfigNotifier(logger)
			gatewayClientsChangesNotifier := newMockGatewayClientsNotifier()

			nodeAgent := konnect.NewNodeAgent(
				tc.hostname, testKicVersion,
				konnect.DefaultRefreshNodePeriod,
				logger,
				nodeClient,
				configStatusSubscriber,
				&mockGatewayInstanceGetter{tc.gatewayInstances},
				gatewayClientsChangesNotifier,
			)

			ctx := context.Background()
			go func() {
				err := nodeAgent.Start(ctx)
				require.NoError(t, err)
			}()

			if tc.configStatus != nil {
				configStatusSubscriber.NotifyConfigStatus(ctx, *tc.configStatus)
			}

			gatewayClientsChangesNotifier.Notify()
			require.Eventually(t, func() bool {
				// check number of nodes in RG.
				nodes := nodeService.dumpNodes()
				if len(nodes) != tc.numNodes {
					return false
				}
				// check for nodes that must be included in RG by hostname, type, version and status.
				for _, node := range tc.containNodes {
					if !lo.ContainsBy(
						nodes,
						func(n *konnect.NodeItem) bool {
							return n.Hostname == node.Hostname &&
								n.Type == node.Type &&
								n.Version == node.Version &&
								n.Status == node.Status
						}) {
						return false
					}
				}
				// check for nodes that must not be included by hostname and type.
				for _, node := range tc.notContainNodes {
					if lo.ContainsBy(
						nodes,
						func(n *konnect.NodeItem) bool {
							return n.Hostname == node.Hostname && n.Type == node.Type
						}) {
						return false
					}
				}

				return true
			}, 2*time.Second, 100*time.Millisecond)
		})
	}
}

func TestNodeAgent_StartDoesntReturnUntilContextGetsCancelled(t *testing.T) {
	t.Parallel()

	nodeService := &mockKonnectNodeService{
		clusterID: testClusterID,
		// Always return errors from ListNodes to ensure that the agent doesn't propagate it to the Start() caller.
		// ListNodes is the first call made by the agent in Start(), so we care only about this one.
		returnErrorFromListNodes: true,
	}
	s := httptest.NewServer(nodeService)
	nodeClient := &konnect.NodeAPIClient{
		Address:        s.URL,
		RuntimeGroupID: testClusterID,
		Client:         &http.Client{},
	}
	logger := testr.New(t)
	configStatusSubscriber := dataplane.NewChannelConfigNotifier(logger)

	nodeAgent := konnect.NewNodeAgent(
		"hostname", testKicVersion,
		konnect.DefaultRefreshNodePeriod,
		logger,
		nodeClient,
		configStatusSubscriber,
		&mockGatewayInstanceGetter{},
		newMockGatewayClientsNotifier(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	agentReturned := make(chan struct{})
	go func() {
		err := nodeAgent.Start(ctx)
		assert.NoError(t, err, "expected no error even when the context is cancelled")
		close(agentReturned)
	}()

	require.Eventually(t, func() bool {
		return nodeService.WasListNodesCalled()
	}, time.Second, time.Millisecond, "expected list nodes to be called when starting the agent")

	// ensure that after list nodes returned an error, the agent didn't return.
	select {
	case <-agentReturned:
		t.Fatal("expected the agent to not return yet")
	default:
	}

	// Cancel the context and wait for the nodeAgent.Start() to return.
	cancel()
	select {
	case <-time.After(time.Second):
		t.Fatal("expected the agent to return after the context was cancelled")
	case <-agentReturned:
	}
}
