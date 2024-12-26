// Code generated by MockGen. DO NOT EDIT.
// Source: client.go

// Package mock_client is a generated GoMock package.
package mock_client

import (
	context "context"
	http "net/http"
	reflect "reflect"

	swagger "github.com/crusoecloud/client-go/swagger/v1alpha5"
	gomock "github.com/golang/mock/gomock"
)

// MockApiClient is a mock of ApiClient interface.
type MockApiClient struct {
	ctrl     *gomock.Controller
	recorder *MockApiClientMockRecorder
}

// MockApiClientMockRecorder is the mock recorder for MockApiClient.
type MockApiClientMockRecorder struct {
	mock *MockApiClient
}

// NewMockApiClient creates a new mock instance.
func NewMockApiClient(ctrl *gomock.Controller) *MockApiClient {
	mock := &MockApiClient{ctrl: ctrl}
	mock.recorder = &MockApiClientMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockApiClient) EXPECT() *MockApiClientMockRecorder {
	return m.recorder
}

// GetIBNetwork mocks base method.
func (m *MockApiClient) GetIBNetwork(ctx context.Context, projectID, ibPartitionID string) (*swagger.IbPartition, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetIBNetwork", ctx, projectID, ibPartitionID)
	ret0, _ := ret[0].(*swagger.IbPartition)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// GetIBNetwork indicates an expected call of GetIBNetwork.
func (mr *MockApiClientMockRecorder) GetIBNetwork(ctx, projectID, ibPartitionID interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetIBNetwork", reflect.TypeOf((*MockApiClient)(nil).GetIBNetwork), ctx, projectID, ibPartitionID)
}

// GetInstanceByID mocks base method.
func (m *MockApiClient) GetInstanceByID(ctx context.Context, instanceId string) (*swagger.InstanceV1Alpha5, *http.Response, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetInstanceByID", ctx, instanceId)
	ret0, _ := ret[0].(*swagger.InstanceV1Alpha5)
	ret1, _ := ret[1].(*http.Response)
	ret2, _ := ret[2].(error)
	return ret0, ret1, ret2
}

// GetInstanceByID indicates an expected call of GetInstanceByID.
func (mr *MockApiClientMockRecorder) GetInstanceByID(ctx, instanceId interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetInstanceByID", reflect.TypeOf((*MockApiClient)(nil).GetInstanceByID), ctx, instanceId)
}

// GetInstanceByName mocks base method.
func (m *MockApiClient) GetInstanceByName(ctx context.Context, nodeName string) (*swagger.InstanceV1Alpha5, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetInstanceByName", ctx, nodeName)
	ret0, _ := ret[0].(*swagger.InstanceV1Alpha5)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// GetInstanceByName indicates an expected call of GetInstanceByName.
func (mr *MockApiClientMockRecorder) GetInstanceByName(ctx, nodeName interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetInstanceByName", reflect.TypeOf((*MockApiClient)(nil).GetInstanceByName), ctx, nodeName)
}
