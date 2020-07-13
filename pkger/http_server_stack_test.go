package pkger_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/influxdata/influxdb/v2"
	"github.com/influxdata/influxdb/v2/pkg/testttp"
	"github.com/influxdata/influxdb/v2/pkger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestPkgerHTTPServerStacks(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b, err := ioutil.ReadFile(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Write(b)
	})
	filesvr := httptest.NewServer(mux)
	defer filesvr.Close()

	strPtr := func(s string) *string {
		return &s
	}

	t.Run("create a stack", func(t *testing.T) {
		t.Run("should successfully return with valid req body", func(t *testing.T) {
			svc := &fakeSVC{
				initStackFn: func(ctx context.Context, userID influxdb.ID, stackCr pkger.StackCreate) (pkger.Stack, error) {
					return pkger.Stack{
						ID:    2,
						OrgID: stackCr.OrgID,
						Events: []pkger.StackEvent{
							{
								Name:         stackCr.Name,
								Description:  stackCr.Description,
								Sources:      stackCr.Sources,
								TemplateURLs: stackCr.TemplateURLs,
								UpdatedAt:    time.Now(),
							},
						},
						CreatedAt: time.Now(),
					}, nil
				},
			}
			pkgHandler := pkger.NewHTTPServerStacks(zap.NewNop(), svc)
			svr := newMountedHandler(pkgHandler, 1)

			reqBody := pkger.ReqCreateStack{
				OrgID:       influxdb.ID(3).String(),
				Name:        "threeve",
				Description: "desc",
				URLs:        []string{"http://example.com"},
			}

			testttp.
				PostJSON(t, "/api/v2/stacks", reqBody).
				Headers("Content-Type", "application/json").
				Do(svr).
				ExpectStatus(http.StatusCreated).
				ExpectBody(func(buf *bytes.Buffer) {
					var resp pkger.RespStack
					decodeBody(t, buf, &resp)

					assert.NotZero(t, resp.ID)
					assert.Equal(t, pkger.StackEventCreate.String(), resp.EventType)
					assert.Equal(t, reqBody.OrgID, resp.OrgID)
					assert.Equal(t, reqBody.Name, resp.Name)
					assert.Equal(t, reqBody.Description, resp.Description)
					assert.Equal(t, reqBody.URLs, resp.URLs)
					assert.NotZero(t, resp.CreatedAt)
					assert.NotZero(t, resp.UpdatedAt)
				})

		})

		t.Run("error cases", func(t *testing.T) {
			tests := []struct {
				name           string
				reqBody        pkger.ReqCreateStack
				expectedStatus int
				svc            pkger.SVC
			}{
				{
					name: "bad org id",
					reqBody: pkger.ReqCreateStack{
						OrgID: "invalid id",
					},
					expectedStatus: http.StatusBadRequest,
				},
				{
					name: "bad url",
					reqBody: pkger.ReqCreateStack{
						OrgID: influxdb.ID(3).String(),
						URLs:  []string{"invalid @% url"},
					},
					expectedStatus: http.StatusBadRequest,
				},
				{
					name:    "translates svc conflict error",
					reqBody: pkger.ReqCreateStack{OrgID: influxdb.ID(3).String()},
					svc: &fakeSVC{
						initStackFn: func(ctx context.Context, userID influxdb.ID, stack pkger.StackCreate) (pkger.Stack, error) {
							return pkger.Stack{}, &influxdb.Error{Code: influxdb.EConflict}
						},
					},
					expectedStatus: http.StatusUnprocessableEntity,
				},
				{
					name:    "translates svc internal error",
					reqBody: pkger.ReqCreateStack{OrgID: influxdb.ID(3).String()},
					svc: &fakeSVC{
						initStackFn: func(ctx context.Context, userID influxdb.ID, stack pkger.StackCreate) (pkger.Stack, error) {
							return pkger.Stack{}, &influxdb.Error{Code: influxdb.EInternal}
						},
					},
					expectedStatus: http.StatusInternalServerError,
				},
			}

			for _, tt := range tests {
				fn := func(t *testing.T) {
					svc := tt.svc
					if svc == nil {
						svc = &fakeSVC{
							initStackFn: func(ctx context.Context, userID influxdb.ID, stack pkger.StackCreate) (pkger.Stack, error) {
								return pkger.Stack{}, nil
							},
						}
					}

					pkgHandler := pkger.NewHTTPServerStacks(zap.NewNop(), svc)
					svr := newMountedHandler(pkgHandler, 1)

					testttp.
						PostJSON(t, "/api/v2/stacks", tt.reqBody).
						Headers("Content-Type", "application/json").
						Do(svr).
						ExpectStatus(tt.expectedStatus)
				}

				t.Run(tt.name, fn)
			}
		})
	})

	t.Run("list a stack", func(t *testing.T) {
		t.Run("should successfully return with valid req body", func(t *testing.T) {
			const expectedOrgID influxdb.ID = 3

			svc := &fakeSVC{
				listStacksFn: func(ctx context.Context, orgID influxdb.ID, filter pkger.ListFilter) ([]pkger.Stack, error) {
					if orgID != expectedOrgID {
						return nil, nil
					}

					if len(filter.Names) > 0 && len(filter.StackIDs) == 0 {
						var stacks []pkger.Stack
						for i, name := range filter.Names {
							stacks = append(stacks, pkger.Stack{
								ID:    influxdb.ID(i + 1),
								OrgID: expectedOrgID,
								Events: []pkger.StackEvent{
									{
										Name: name,
									},
								},
							})
						}
						return stacks, nil
					}

					if len(filter.StackIDs) > 0 && len(filter.Names) == 0 {
						var stacks []pkger.Stack
						for _, stackID := range filter.StackIDs {
							stacks = append(stacks, pkger.Stack{
								ID:     stackID,
								OrgID:  expectedOrgID,
								Events: []pkger.StackEvent{{}},
							})
						}
						return stacks, nil
					}

					return []pkger.Stack{{
						ID:    1,
						OrgID: expectedOrgID,
						Events: []pkger.StackEvent{{
							Name: "stack_1",
						}},
					}}, nil
				},
			}

			pkgHandler := pkger.NewHTTPServerStacks(zap.NewNop(), svc)
			svr := newMountedHandler(pkgHandler, 1)

			tests := []struct {
				name           string
				queryArgs      string
				expectedStacks []pkger.RespStack
			}{
				{
					name:      "with org ID that has stacks",
					queryArgs: "orgID=" + expectedOrgID.String(),
					expectedStacks: []pkger.RespStack{{
						ID:    influxdb.ID(1).String(),
						OrgID: expectedOrgID.String(),
						RespStackEvent: pkger.RespStackEvent{
							EventType: pkger.StackEventCreate.String(),
							Name:      "stack_1",
							Resources: []pkger.RespStackResource{},
							Sources:   []string{},
							URLs:      []string{},
						},
						Events: []pkger.RespStackEvent{
							{
								EventType: pkger.StackEventCreate.String(),
								Name:      "stack_1",
								Resources: []pkger.RespStackResource{},
								Sources:   []string{},
								URLs:      []string{},
							},
						},
					}},
				},
				{
					name:           "with orgID with no stacks",
					queryArgs:      "orgID=" + influxdb.ID(9000).String(),
					expectedStacks: []pkger.RespStack{},
				},
				{
					name:      "with names",
					queryArgs: "name=name_stack&name=threeve&orgID=" + expectedOrgID.String(),
					expectedStacks: []pkger.RespStack{
						{
							ID:    influxdb.ID(1).String(),
							OrgID: expectedOrgID.String(),
							RespStackEvent: pkger.RespStackEvent{
								EventType: pkger.StackEventCreate.String(),
								Name:      "name_stack",
								Resources: []pkger.RespStackResource{},
								Sources:   []string{},
								URLs:      []string{},
							},
							Events: []pkger.RespStackEvent{
								{
									EventType: pkger.StackEventCreate.String(),
									Name:      "name_stack",
									Resources: []pkger.RespStackResource{},
									Sources:   []string{},
									URLs:      []string{},
								},
							},
						},
						{
							ID:    influxdb.ID(2).String(),
							OrgID: expectedOrgID.String(),
							RespStackEvent: pkger.RespStackEvent{
								EventType: pkger.StackEventCreate.String(),
								Name:      "threeve",
								Resources: []pkger.RespStackResource{},
								Sources:   []string{},
								URLs:      []string{},
							},
							Events: []pkger.RespStackEvent{
								{
									EventType: pkger.StackEventCreate.String(),
									Name:      "threeve",
									Resources: []pkger.RespStackResource{},
									Sources:   []string{},
									URLs:      []string{},
								},
							},
						},
					},
				},
				{
					name:      "with ids",
					queryArgs: fmt.Sprintf("stackID=%s&stackID=%s&orgID=%s", influxdb.ID(1), influxdb.ID(2), influxdb.ID(expectedOrgID)),
					expectedStacks: []pkger.RespStack{
						{
							ID:    influxdb.ID(1).String(),
							OrgID: expectedOrgID.String(),
							RespStackEvent: pkger.RespStackEvent{
								EventType: pkger.StackEventCreate.String(),
								Resources: []pkger.RespStackResource{},
								Sources:   []string{},
								URLs:      []string{},
							},
							Events: []pkger.RespStackEvent{
								{
									EventType: pkger.StackEventCreate.String(),
									Resources: []pkger.RespStackResource{},
									Sources:   []string{},
									URLs:      []string{},
								},
							},
						},
						{
							ID:    influxdb.ID(2).String(),
							OrgID: expectedOrgID.String(),
							RespStackEvent: pkger.RespStackEvent{
								EventType: pkger.StackEventCreate.String(),
								Resources: []pkger.RespStackResource{},
								Sources:   []string{},
								URLs:      []string{},
							},
							Events: []pkger.RespStackEvent{
								{
									EventType: pkger.StackEventCreate.String(),
									Resources: []pkger.RespStackResource{},
									Sources:   []string{},
									URLs:      []string{},
								},
							},
						},
					},
				},
			}

			for _, tt := range tests {
				fn := func(t *testing.T) {
					testttp.
						Get(t, "/api/v2/stacks?"+tt.queryArgs).
						Headers("Content-Type", "application/x-www-form-urlencoded").
						Do(svr).
						ExpectStatus(http.StatusOK).
						ExpectBody(func(buf *bytes.Buffer) {
							var resp pkger.RespListStacks
							decodeBody(t, buf, &resp)

							assert.Equal(t, tt.expectedStacks, resp.Stacks)
						})
				}

				t.Run(tt.name, fn)
			}
		})
	})

	t.Run("read a stack", func(t *testing.T) {
		t.Run("should successfully return with valid req body", func(t *testing.T) {
			const expectedOrgID influxdb.ID = 3

			tests := []struct {
				name          string
				stub          pkger.Stack
				expectedStack pkger.RespStack
			}{
				{
					name: "for stack that has all fields available",
					stub: pkger.Stack{
						ID:    1,
						OrgID: expectedOrgID,
						Events: []pkger.StackEvent{
							{
								Name:         "name",
								Description:  "desc",
								Sources:      []string{"threeve"},
								TemplateURLs: []string{"http://example.com"},
								Resources: []pkger.StackResource{
									{
										APIVersion: pkger.APIVersion,
										ID:         3,
										Kind:       pkger.KindBucket,
										MetaName:   "rucketeer",
									},
								},
							},
						},
					},
					expectedStack: pkger.RespStack{
						ID:    influxdb.ID(1).String(),
						OrgID: expectedOrgID.String(),
						RespStackEvent: pkger.RespStackEvent{
							EventType:   pkger.StackEventCreate.String(),
							Name:        "name",
							Description: "desc",
							Sources:     []string{"threeve"},
							URLs:        []string{"http://example.com"},
							Resources: []pkger.RespStackResource{
								{
									APIVersion:   pkger.APIVersion,
									ID:           influxdb.ID(3).String(),
									Kind:         pkger.KindBucket,
									MetaName:     "rucketeer",
									Associations: []pkger.RespStackResourceAssoc{},
								},
							},
						},
						Events: []pkger.RespStackEvent{
							{
								EventType:   pkger.StackEventCreate.String(),
								Name:        "name",
								Description: "desc",
								Sources:     []string{"threeve"},
								URLs:        []string{"http://example.com"},
								Resources: []pkger.RespStackResource{
									{
										APIVersion:   pkger.APIVersion,
										ID:           influxdb.ID(3).String(),
										Kind:         pkger.KindBucket,
										MetaName:     "rucketeer",
										Associations: []pkger.RespStackResourceAssoc{},
									},
								},
							},
						},
					},
				},
				{
					name: "for stack that has missing resources urls and sources",
					stub: pkger.Stack{
						ID:    1,
						OrgID: expectedOrgID,
						Events: []pkger.StackEvent{
							{
								Name:        "name",
								Description: "desc",
							},
						},
					},
					expectedStack: pkger.RespStack{
						ID:    influxdb.ID(1).String(),
						OrgID: expectedOrgID.String(),
						RespStackEvent: pkger.RespStackEvent{
							EventType:   pkger.StackEventCreate.String(),
							Name:        "name",
							Description: "desc",
							Sources:     []string{},
							URLs:        []string{},
							Resources:   []pkger.RespStackResource{},
						},
						Events: []pkger.RespStackEvent{
							{
								EventType:   pkger.StackEventCreate.String(),
								Name:        "name",
								Description: "desc",
								Sources:     []string{},
								URLs:        []string{},
								Resources:   []pkger.RespStackResource{},
							},
						},
					},
				},
				{
					name: "for stack that has no set fields",
					stub: pkger.Stack{
						ID:     1,
						OrgID:  expectedOrgID,
						Events: []pkger.StackEvent{{}},
					},
					expectedStack: pkger.RespStack{
						ID:    influxdb.ID(1).String(),
						OrgID: expectedOrgID.String(),
						RespStackEvent: pkger.RespStackEvent{
							EventType: pkger.StackEventCreate.String(),
							Sources:   []string{},
							URLs:      []string{},
							Resources: []pkger.RespStackResource{},
						},
						Events: []pkger.RespStackEvent{
							{
								EventType: pkger.StackEventCreate.String(),
								Sources:   []string{},
								URLs:      []string{},
								Resources: []pkger.RespStackResource{},
							},
						},
					},
				},
			}

			for _, tt := range tests {
				fn := func(t *testing.T) {
					svc := &fakeSVC{
						readStackFn: func(ctx context.Context, id influxdb.ID) (pkger.Stack, error) {
							return tt.stub, nil
						},
					}
					pkgHandler := pkger.NewHTTPServerStacks(zap.NewNop(), svc)
					svr := newMountedHandler(pkgHandler, 1)

					testttp.
						Get(t, "/api/v2/stacks/"+tt.stub.ID.String()).
						Do(svr).
						ExpectStatus(http.StatusOK).
						ExpectBody(func(buf *bytes.Buffer) {
							var resp pkger.RespStack
							decodeBody(t, buf, &resp)

							assert.Equal(t, tt.expectedStack, resp)
						})
				}

				t.Run(tt.name, fn)
			}
		})

		t.Run("error cases", func(t *testing.T) {
			tests := []struct {
				name           string
				stackIDPath    string
				expectedStatus int
				svc            pkger.SVC
			}{
				{
					name:           "bad stack id path",
					stackIDPath:    "badID",
					expectedStatus: http.StatusBadRequest,
				},
				{
					name:        "stack not found",
					stackIDPath: influxdb.ID(1).String(),
					svc: &fakeSVC{
						readStackFn: func(ctx context.Context, id influxdb.ID) (pkger.Stack, error) {
							return pkger.Stack{}, &influxdb.Error{Code: influxdb.ENotFound}
						},
					},
					expectedStatus: http.StatusNotFound,
				},
			}

			for _, tt := range tests {
				fn := func(t *testing.T) {
					svc := tt.svc
					if svc == nil {
						svc = &fakeSVC{
							initStackFn: func(ctx context.Context, userID influxdb.ID, stack pkger.StackCreate) (pkger.Stack, error) {
								return pkger.Stack{}, nil
							},
						}
					}

					pkgHandler := pkger.NewHTTPServerStacks(zap.NewNop(), svc)
					svr := newMountedHandler(pkgHandler, 1)

					testttp.
						Get(t, "/api/v2/stacks/"+tt.stackIDPath).
						Headers("Content-Type", "application/json").
						Do(svr).
						ExpectStatus(tt.expectedStatus)
				}

				t.Run(tt.name, fn)
			}
		})
	})

	t.Run("update a stack", func(t *testing.T) {
		t.Run("should successfully update with valid req body", func(t *testing.T) {
			const expectedOrgID influxdb.ID = 3

			tests := []struct {
				name          string
				input         pkger.ReqUpdateStack
				expectedStack pkger.RespStack
			}{
				{
					name: "update name field",
					input: pkger.ReqUpdateStack{
						Name: strPtr("name"),
					},
					expectedStack: pkger.RespStack{
						ID:    influxdb.ID(1).String(),
						OrgID: expectedOrgID.String(),
						RespStackEvent: pkger.RespStackEvent{
							EventType: pkger.StackEventUpdate.String(),
							Name:      "name",
							Sources:   []string{},
							URLs:      []string{},
							Resources: []pkger.RespStackResource{},
						},
						Events: []pkger.RespStackEvent{
							{
								EventType: pkger.StackEventUpdate.String(),
								Name:      "name",
								Sources:   []string{},
								URLs:      []string{},
								Resources: []pkger.RespStackResource{},
							},
						},
					},
				},
				{
					name: "update desc field",
					input: pkger.ReqUpdateStack{
						Description: strPtr("desc"),
					},
					expectedStack: pkger.RespStack{
						ID:    influxdb.ID(1).String(),
						OrgID: expectedOrgID.String(),
						RespStackEvent: pkger.RespStackEvent{
							EventType:   pkger.StackEventUpdate.String(),
							Description: "desc",
							Sources:     []string{},
							URLs:        []string{},
							Resources:   []pkger.RespStackResource{},
						},
						Events: []pkger.RespStackEvent{
							{
								EventType:   pkger.StackEventUpdate.String(),
								Description: "desc",
								Sources:     []string{},
								URLs:        []string{},
								Resources:   []pkger.RespStackResource{},
							},
						},
					},
				},
				{
					name: "update urls field",
					input: pkger.ReqUpdateStack{
						TemplateURLs: []string{"http://example.com"},
					},
					expectedStack: pkger.RespStack{
						ID:    influxdb.ID(1).String(),
						OrgID: expectedOrgID.String(),
						RespStackEvent: pkger.RespStackEvent{
							EventType: pkger.StackEventUpdate.String(),
							Sources:   []string{},
							URLs:      []string{"http://example.com"},
							Resources: []pkger.RespStackResource{},
						},
						Events: []pkger.RespStackEvent{
							{
								EventType: pkger.StackEventUpdate.String(),
								Sources:   []string{},
								URLs:      []string{"http://example.com"},
								Resources: []pkger.RespStackResource{},
							},
						},
					},
				},
				{
					name: "update all fields",
					input: pkger.ReqUpdateStack{
						Name:         strPtr("name"),
						Description:  strPtr("desc"),
						TemplateURLs: []string{"http://example.com"},
					},
					expectedStack: pkger.RespStack{
						ID:    influxdb.ID(1).String(),
						OrgID: expectedOrgID.String(),
						RespStackEvent: pkger.RespStackEvent{
							EventType:   pkger.StackEventUpdate.String(),
							Name:        "name",
							Description: "desc",
							Sources:     []string{},
							URLs:        []string{"http://example.com"},
							Resources:   []pkger.RespStackResource{},
						},
						Events: []pkger.RespStackEvent{
							{
								EventType:   pkger.StackEventUpdate.String(),
								Name:        "name",
								Description: "desc",
								Sources:     []string{},
								URLs:        []string{"http://example.com"},
								Resources:   []pkger.RespStackResource{},
							},
						},
					},
				},
			}

			for _, tt := range tests {
				fn := func(t *testing.T) {
					id, err := influxdb.IDFromString(tt.expectedStack.ID)
					require.NoError(t, err)

					svc := &fakeSVC{
						updateStackFn: func(ctx context.Context, upd pkger.StackUpdate) (pkger.Stack, error) {
							if upd.ID != *id {
								return pkger.Stack{}, errors.New("unexpected stack ID: " + upd.ID.String())
							}
							st := pkger.Stack{
								ID:    *id,
								OrgID: expectedOrgID,
							}
							ev := pkger.StackEvent{EventType: pkger.StackEventUpdate}
							if upd.Name != nil {
								ev.Name = *upd.Name
							}
							if upd.Description != nil {
								ev.Description = *upd.Description
							}
							if upd.TemplateURLs != nil {
								ev.TemplateURLs = upd.TemplateURLs
							}
							st.Events = append(st.Events, ev)
							return st, nil
						},
					}
					pkgHandler := pkger.NewHTTPServerStacks(zap.NewNop(), svc)
					svr := newMountedHandler(pkgHandler, 1)

					testttp.
						PatchJSON(t, "/api/v2/stacks/"+tt.expectedStack.ID, tt.input).
						Do(svr).
						ExpectStatus(http.StatusOK).
						ExpectBody(func(buf *bytes.Buffer) {
							var resp pkger.RespStack
							decodeBody(t, buf, &resp)

							assert.Equal(t, tt.expectedStack, resp)
						})
				}

				t.Run(tt.name, fn)
			}
		})

		t.Run("error cases", func(t *testing.T) {
			tests := []struct {
				name           string
				stackIDPath    string
				expectedStatus int
				svc            pkger.SVC
			}{
				{
					name:           "bad stack id path",
					stackIDPath:    "badID",
					expectedStatus: http.StatusBadRequest,
				},
				{
					name:        "stack not found",
					stackIDPath: influxdb.ID(1).String(),
					svc: &fakeSVC{
						readStackFn: func(ctx context.Context, id influxdb.ID) (pkger.Stack, error) {
							return pkger.Stack{}, &influxdb.Error{Code: influxdb.ENotFound}
						},
					},
					expectedStatus: http.StatusNotFound,
				},
			}

			for _, tt := range tests {
				fn := func(t *testing.T) {
					svc := tt.svc
					if svc == nil {
						svc = &fakeSVC{
							initStackFn: func(ctx context.Context, userID influxdb.ID, stack pkger.StackCreate) (pkger.Stack, error) {
								return pkger.Stack{}, nil
							},
						}
					}

					pkgHandler := pkger.NewHTTPServerStacks(zap.NewNop(), svc)
					svr := newMountedHandler(pkgHandler, 1)

					testttp.
						Get(t, "/api/v2/stacks/"+tt.stackIDPath).
						Headers("Content-Type", "application/json").
						Do(svr).
						ExpectStatus(tt.expectedStatus)
				}

				t.Run(tt.name, fn)
			}
		})
	})
}
