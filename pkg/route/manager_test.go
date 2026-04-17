package route

import (
	"fmt"
	"testing"
)

func Test_SingleAddDel(t *testing.T) {
	t.Run("Add and delete single route", func(_ *testing.T) {
		m := NewManager()
		if m == nil { // nolint
			t.Error("failed to create manager")
		}

		r := &mockRoute{hash: "123456"}

		name := "test-service"

		if err := m.Add(name, r, false, false); err != nil {
			t.Errorf("add failed for route %q, object %q", r.RouteHash(), name)
		}

		expected := 1
		if len(m.tracker) != expected { //nolint
			t.Errorf("number of items in tracker should be %d, but is %d", expected, len(m.tracker))
		}

		item, exists := m.tracker[r.RouteHash()]
		if !exists {
			t.Errorf("object %q was not added to tracker for service %q", r.RouteHash(), name)
		}

		if len(item.objects) != 1 {
			t.Errorf("number of objects for %q was %d instead of 1", name, len(item.objects))
		}

		if err := m.Delete(name, r); err != nil {
			t.Errorf("deletion of object %q failed for service %q", r.RouteHash(), name)
		}

		expected = 0
		if len(m.tracker) != expected {
			t.Errorf("number of items in tracker should be %d, but is %d", expected, len(m.tracker))
		}
	})
}

func Test_MultipleAddDel(t *testing.T) {
	t.Run("Add and delete same route multiple times", func(_ *testing.T) {
		m := NewManager()
		if m == nil {
			t.Error("failed to create manager")
		}

		r := &mockRoute{hash: "123456"}

		numOfServices := 50
		expectedRoutes := 1

		for i := range numOfServices {
			name := fmt.Sprintf("test-service-%d", i)

			if err := m.Add(name, r, false, false); err != nil {
				t.Errorf("add failed for route %q, object %q", r.RouteHash(), name)
			}

			if len(m.tracker) != expectedRoutes {
				t.Errorf("number of routes in tracker should be %d, but is %d", expectedRoutes, len(m.tracker))
			}

			item, exists := m.tracker[r.RouteHash()]
			if !exists {
				t.Errorf("route %q was not added to the tracker", r.RouteHash())
			}

			expected := i + 1
			if len(item.objects) != expected {
				t.Errorf("number of objects for %q was %d instead of 1", name, len(item.objects))
			}
		}

		for i := range numOfServices {
			name := fmt.Sprintf("test-service-%d", i)

			if err := m.Delete(name, r); err != nil {
				t.Errorf("delete failed for route %q, object %q", r.RouteHash(), name)
			}

			if i == numOfServices-1 {
				expectedRoutes = 0
			}

			if len(m.tracker) != expectedRoutes {
				t.Errorf("number of routes in tracker should be %d, but is %d", expectedRoutes, len(m.tracker))
			}

			exists := m.Check(r.RouteHash())
			if i < numOfServices-1 {
				if !exists {
					t.Errorf("route %q was not added to the tracker", r.RouteHash())
				}
			} else {
				if exists {
					t.Errorf("route %q was not deleted from the tracker", r.RouteHash())
				}
			}
		}
	})
}

func Test_MultipleRoutesAddDel(t *testing.T) {
	t.Run("Add and delete multiple routes multiple times", func(_ *testing.T) {
		m := NewManager()
		if m == nil {
			t.Error("failed to create manager")
		}

		routes := []*mockRoute{}

		numOfRoutes := 20
		hashBase := 123456

		for i := range numOfRoutes {
			routes = append(routes, &mockRoute{hash: fmt.Sprintf("%d%d", hashBase, i)})
		}

		numOfServices := 50

		for rtCnt, r := range routes {
			for i := range numOfServices {

				expectedRoutes := rtCnt + 1
				name := fmt.Sprintf("test-service-%d", i)

				if err := m.Add(name, r, false, false); err != nil {
					t.Errorf("add failed for route %q, object %q", r.RouteHash(), name)
				}

				if len(m.tracker) != expectedRoutes {
					t.Errorf("number of routes in tracker should be %d, but is %d", expectedRoutes, len(m.tracker))
				}

				item, exists := m.tracker[r.RouteHash()]
				if !exists {
					t.Errorf("route %q was not added to the tracker", r.RouteHash())
				}

				expected := i + 1
				if len(item.objects) != expected {
					t.Errorf("number of objects for %q was %d instead of 1", name, len(item.objects))
				}
			}
		}

		for rtCnt, r := range routes {
			for i := range numOfServices {
				name := fmt.Sprintf("test-service-%d", i)

				if err := m.Delete(name, r); err != nil {
					t.Errorf("delete failed for route %q, object %q", r.RouteHash(), name)
				}

				expectedRoutes := numOfRoutes - rtCnt
				if i == numOfServices-1 {
					expectedRoutes -= 1
				}

				if len(m.tracker) != expectedRoutes {
					t.Errorf("number of routes in tracker should be %d, but is %d", expectedRoutes, len(m.tracker))
				}

				exists := m.Check(r.RouteHash())
				if i < numOfServices-1 {
					if !exists {
						t.Errorf("route %q should not be deleted", r.RouteHash())
					}
				} else {
					if exists {
						t.Errorf("route %q was not deleted from the tracker", r.RouteHash())
					}
				}
			}
		}
	})
}

type mockRoute struct {
	added     bool
	addErr    error
	updated   bool
	updateErr error
	delErr    error
	hash      string
	intf      string
}

func (mr *mockRoute) AddRoute(_ bool) (bool, error) {
	return mr.added, mr.addErr
}

func (mr *mockRoute) UpdateRoutes() (bool, error) {
	return mr.updated, mr.updateErr
}

func (mr *mockRoute) DeleteRoute() error {
	return mr.delErr
}

func (mr *mockRoute) RouteHash() string {
	return mr.hash
}

func (mr *mockRoute) Interface() string {
	return mr.intf
}
