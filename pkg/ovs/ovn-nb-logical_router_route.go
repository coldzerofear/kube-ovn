package ovs

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/scylladb/go-set/strset"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"k8s.io/utils/set"

	ovsclient "github.com/kubeovn/kube-ovn/pkg/ovsdb/client"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnnb"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

func (c *OVNNbClient) ListLogicalRouterStaticRoutesByOption(lrName, _, key, value string) ([]*ovnnb.LogicalRouterStaticRoute, error) {
	fnFilter := func(route *ovnnb.LogicalRouterStaticRoute) bool {
		if len(route.Options) != 0 {
			if _, ok := route.Options[key]; ok {
				return route.Options[key] == value
			}
		}
		return false
	}
	return c.listLogicalRouterStaticRoutesByFilter(lrName, fnFilter)
}

// CreateLogicalRouterStaticRoutes create several logical router static route once
func (c *OVNNbClient) CreateLogicalRouterStaticRoutes(lrName string, routes ...*ovnnb.LogicalRouterStaticRoute) error {
	if len(routes) == 0 {
		return nil
	}

	models := make([]model.Model, 0, len(routes))
	routeUUIDs := make([]string, 0, len(routes))
	for _, route := range routes {
		if route != nil {
			models = append(models, model.Model(route))
			routeUUIDs = append(routeUUIDs, route.UUID)
		}
	}

	createRoutesOp, err := c.Create(models...)
	if err != nil {
		klog.Error(err)
		return fmt.Errorf("generate operations for creating static routes: %w", err)
	}

	routeAddOp, err := c.LogicalRouterUpdateStaticRouteOp(lrName, routeUUIDs, ovsdb.MutateOperationInsert)
	if err != nil {
		klog.Error(err)
		return fmt.Errorf("generate operations for adding static routes to logical router %s: %w", lrName, err)
	}

	ops := make([]ovsdb.Operation, 0, len(createRoutesOp)+len(routeAddOp))
	ops = append(ops, createRoutesOp...)
	ops = append(ops, routeAddOp...)

	if err = c.Transact("lr-routes-add", ops); err != nil {
		klog.Error(err)
		return fmt.Errorf("add static routes to %s: %w", lrName, err)
	}

	return nil
}

// AddLogicalRouterStaticRoute add a logical router static route
func (c *OVNNbClient) AddLogicalRouterStaticRoute(lrName, routeTable, policy, ipPrefix string, bfdID *string, externalIDs map[string]string, nexthops ...string) error {
	if len(policy) == 0 {
		policy = ovnnb.LogicalRouterStaticRoutePolicyDstIP
	}

	routes, err := c.ListLogicalRouterStaticRoutes(lrName, &routeTable, &policy, ipPrefix, nil)
	if err != nil {
		klog.Error(err)
		return err
	}

	existing := strset.New()
	var toDel []string
	for _, route := range routes {
		if slices.Contains(nexthops, route.Nexthop) {
			existing.Add(route.Nexthop)
		} else {
			if route.BFD != nil && bfdID != nil && *route.BFD != *bfdID {
				continue
			}
			toDel = append(toDel, route.UUID)
		}
	}
	var toAdd []*ovnnb.LogicalRouterStaticRoute
	for _, nexthop := range nexthops {
		if !existing.Has(nexthop) {
			route, err := c.newLogicalRouterStaticRoute(lrName, routeTable, policy, ipPrefix, nexthop, bfdID, externalIDs)
			if err != nil {
				klog.Error(err)
				return err
			}
			toAdd = append(toAdd, route)
		}
	}
	if len(toDel) != 0 {
		klog.Infof("logical router %s del static routes: %v", lrName, toDel)
	}
	ops, err := c.LogicalRouterUpdateStaticRouteOp(lrName, toDel, ovsdb.MutateOperationDelete)
	if err != nil {
		klog.Error(err)
		return fmt.Errorf("generate operations for removing static routes from logical router %s: %w", lrName, err)
	}
	if err = c.Transact("lr-route-del", ops); err != nil {
		klog.Error(err)
		return fmt.Errorf("failed to delete static routes from logical router %s: %w", lrName, err)
	}
	if err = c.CreateLogicalRouterStaticRoutes(lrName, toAdd...); err != nil {
		klog.Error(err)
		return fmt.Errorf("failed to add static routes to logical router %s: %w", lrName, err)
	}
	return nil
}

// UpdateLogicalRouterStaticRoute update logical router static route
func (c *OVNNbClient) UpdateLogicalRouterStaticRoute(route *ovnnb.LogicalRouterStaticRoute, fields ...interface{}) error {
	if route == nil {
		return errors.New("route is nil")
	}

	op, err := c.ovsDbClient.Where(route).Update(route, fields...)
	if err != nil {
		klog.Error(err)
		return fmt.Errorf("generate operations for updating logical router static route 'policy %s ip_prefix %s': %w", *route.Policy, route.IPPrefix, err)
	}

	if err = c.Transact("net-update", op); err != nil {
		klog.Error(err)
		return fmt.Errorf("update logical router static route 'policy %s ip_prefix %s': %w", *route.Policy, route.IPPrefix, err)
	}

	return nil
}

// DeleteLogicalRouterStaticRoute delete a logical router static route
func (c *OVNNbClient) DeleteLogicalRouterStaticRoute(lrName string, routeTable, policy *string, ipPrefix, nexthop string) error {
	if policy == nil || len(*policy) == 0 {
		policy = ptr.To(ovnnb.LogicalRouterStaticRoutePolicyDstIP)
	}

	lr, err := c.GetLogicalRouter(lrName, true)
	if lr == nil && err == nil {
		return nil
	}

	routes, err := c.ListLogicalRouterStaticRoutes(lrName, routeTable, policy, ipPrefix, nil)
	if err != nil {
		klog.Error(err)
		return err
	}

	// not found, skip
	if len(routes) == 0 {
		return nil
	}

	uuids := make([]string, 0, len(routes))
	for _, route := range routes {
		if nexthop == "" || route.Nexthop == nexthop {
			uuids = append(uuids, route.UUID)
		}
	}

	// remove static route from logical router
	ops, err := c.LogicalRouterUpdateStaticRouteOp(lrName, uuids, ovsdb.MutateOperationDelete)
	if err != nil {
		klog.Error(err)
		return fmt.Errorf("generate operations for removing static routes %v from logical router %s: %w", uuids, lrName, err)
	}
	if err = c.Transact("lr-route-del", ops); err != nil {
		klog.Error(err)
		return fmt.Errorf("delete static routes %v from logical router %s: %w", uuids, lrName, err)
	}

	return nil
}

// DeleteLogicalRouterStaticRoute delete a logical router static route
func (c *OVNNbClient) DeleteLogicalRouterStaticRouteByUUID(lrName, uuid string) error {
	lr, err := c.GetLogicalRouter(lrName, true)
	if err != nil {
		return err
	}
	if lr == nil {
		return nil
	}

	// remove static route from logical router
	ops, err := c.LogicalRouterUpdateStaticRouteOp(lrName, []string{uuid}, ovsdb.MutateOperationDelete)
	if err != nil {
		klog.Error(err)
		return fmt.Errorf("generate operations for removing static route %s from logical router %s: %w", uuid, lrName, err)
	}
	if err = c.Transact("lr-route-del", ops); err != nil {
		klog.Error(err)
		return fmt.Errorf("delete static route %s from logical router %s: %w", uuid, lrName, err)
	}

	return nil
}

func (c *OVNNbClient) DeleteLogicalRouterStaticRouteByExternalIDs(lrName string, externalIDs map[string]string) error {
	lr, err := c.GetLogicalRouter(lrName, true)
	if err != nil {
		return err
	}
	if lr == nil {
		return nil
	}

	routes, err := c.ListLogicalRouterStaticRoutes(lrName, nil, nil, "", externalIDs)
	if err != nil {
		klog.Error(err)
		return err
	}
	if len(routes) == 0 {
		return nil
	}

	uuids := make([]string, 0, len(routes))
	for _, route := range routes {
		uuids = append(uuids, route.UUID)
	}

	// remove static route from logical router
	ops, err := c.LogicalRouterUpdateStaticRouteOp(lrName, uuids, ovsdb.MutateOperationDelete)
	if err != nil {
		klog.Error(err)
		return fmt.Errorf("generate operations for removing static routes %v from logical router %s: %w", uuids, lrName, err)
	}
	if err = c.Transact("lr-route-del", ops); err != nil {
		klog.Error(err)
		return fmt.Errorf("delete static routes %v from logical router %s: %w", uuids, lrName, err)
	}

	return nil
}

// BatchDeleteLogicalRouterStaticRoute batch delete a logical router static route
func (c *OVNNbClient) BatchDeleteLogicalRouterStaticRoute(lrName string, staticRoutes []*ovnnb.LogicalRouterStaticRoute) error {
	lr, err := c.GetLogicalRouter(lrName, true)
	if lr == nil && err == nil {
		return nil
	}

	staticRoutesMap := make(map[string]string, len(staticRoutes))
	for _, route := range staticRoutes {
		if route == nil {
			continue
		}
		if route.Policy == nil {
			route.Policy = &ovnnb.LogicalRouterStaticRoutePolicyDstIP
		}

		staticRoutesMap[createStaticRouteKey(route.RouteTable, *route.Policy, route.IPPrefix)] = route.Nexthop
	}
	routes, err := c.batchListLogicalRouterStaticRoutesForDelete(staticRoutesMap, lr.StaticRoutes)
	if err != nil {
		klog.Error(err)
		return err
	}

	// not found, skip
	if len(routes) == 0 {
		return nil
	}

	uuids := make([]string, 0, len(routes))
	for _, route := range routes {
		key := createStaticRouteKey(route.RouteTable, *route.Policy, route.IPPrefix)
		nexthop, exits := staticRoutesMap[key]
		if exits && (nexthop == "" || route.Nexthop == nexthop) {
			uuids = append(uuids, route.UUID)
		}
	}

	// remove static route from logical router
	ops, err := c.LogicalRouterUpdateStaticRouteOp(lrName, uuids, ovsdb.MutateOperationDelete)
	if err != nil {
		klog.Error(err)
		return fmt.Errorf("generate operations for removing static routes %v from logical router %s: %w", uuids, lrName, err)
	}
	if err = c.Transact("lr-route-del", ops); err != nil {
		klog.Error(err)
		return fmt.Errorf("delete static routes %v from logical router %s: %w", uuids, lrName, err)
	}

	return nil
}

// ClearLogicalRouterStaticRoute clear static route from logical router once
func (c *OVNNbClient) ClearLogicalRouterStaticRoute(lrName string) error {
	lr, err := c.GetLogicalRouter(lrName, false)
	if err != nil {
		klog.Error(err)
		return fmt.Errorf("get logical router %s: %w", lrName, err)
	}

	// clear static route
	lr.StaticRoutes = nil
	ops, err := c.UpdateLogicalRouterOp(lr, &lr.StaticRoutes)
	if err != nil {
		klog.Error(err)
		return fmt.Errorf("generate operations for clear logical router %s static route: %w", lrName, err)
	}
	if err = c.Transact("lr-route-clear", ops); err != nil {
		klog.Error(err)
		return fmt.Errorf("clear logical router %s static routes: %w", lrName, err)
	}

	return nil
}

// GetLogicalRouterStaticRouteByUUID get logical router static route by UUID
func (c *OVNNbClient) GetLogicalRouterStaticRouteByUUID(uuid string) (*ovnnb.LogicalRouterStaticRoute, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	defer cancel()

	route := &ovnnb.LogicalRouterStaticRoute{UUID: uuid}
	if err := c.Get(ctx, route); err != nil {
		klog.Error(err)
		return nil, err
	}

	return route, nil
}

// GetLogicalRouterStaticRoute get logical router static route by some attribute,
// a static route is uniquely identified by router(lrName), policy and ipPrefix when route is not ecmp
// a static route is uniquely identified by router(lrName), policy, ipPrefix and nexthop when route is ecmp
func (c *OVNNbClient) GetLogicalRouterStaticRoute(lrName, routeTable, policy, ipPrefix, nexthop string, ignoreNotFound bool) (*ovnnb.LogicalRouterStaticRoute, error) {
	// this is necessary because may exist same static route in different logical router
	if len(lrName) == 0 {
		return nil, errors.New("the logical router name is required")
	}

	fnFilter := func(route *ovnnb.LogicalRouterStaticRoute) bool {
		return route.RouteTable == routeTable && route.Policy != nil && *route.Policy == policy && route.IPPrefix == ipPrefix && route.Nexthop == nexthop
	}
	routeList, err := c.listLogicalRouterStaticRoutesByFilter(lrName, fnFilter)
	if err != nil {
		klog.Error(err)
		return nil, fmt.Errorf("get logical router %s static route 'policy %s ip_prefix %s nexthop %s': %w", lrName, policy, ipPrefix, nexthop, err)
	}

	// not found
	if len(routeList) == 0 {
		if ignoreNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("not found logical router %s static route 'policy %s ip_prefix %s nexthop %s'", lrName, policy, ipPrefix, nexthop)
	}

	if len(routeList) > 1 {
		return nil, fmt.Errorf("more than one static route 'policy %s ip_prefix %s nexthop %s' in logical router %s", policy, ipPrefix, nexthop, lrName)
	}

	return routeList[0], nil
}

// ListLogicalRouterStaticRoutes list route which match the given externalIDs
func (c *OVNNbClient) ListLogicalRouterStaticRoutes(lrName string, routeTable, policy *string, ipPrefix string, externalIDs map[string]string) ([]*ovnnb.LogicalRouterStaticRoute, error) {
	fnFilter := func(route *ovnnb.LogicalRouterStaticRoute) bool {
		if len(route.ExternalIDs) < len(externalIDs) {
			return false
		}

		if len(route.ExternalIDs) != 0 {
			for k, v := range externalIDs {
				// if only key exist but not value in externalIDs, we should include this route,
				// it's equal to shell command `ovn-nbctl --columns=xx find logical_router_static_route external_ids:key!=\"\"`
				if len(v) == 0 {
					if len(route.ExternalIDs[k]) == 0 {
						return false
					}
				} else {
					if route.ExternalIDs[k] != v {
						return false
					}
				}
			}
		}

		if routeTable != nil && route.RouteTable != *routeTable {
			return false
		}
		if policy != nil {
			if route.Policy != nil {
				if *route.Policy != *policy {
					return false
				}
			} else if *policy != ovnnb.LogicalRouterStaticRoutePolicyDstIP {
				return false
			}
		}
		if ipPrefix != "" && route.IPPrefix != ipPrefix {
			return false
		}

		return true
	}

	return c.listLogicalRouterStaticRoutesByFilter(lrName, fnFilter)
}

func (c *OVNNbClient) LogicalRouterStaticRouteExists(lrName, routeTable, policy, ipPrefix, nexthop string) (bool, error) {
	route, err := c.GetLogicalRouterStaticRoute(lrName, routeTable, policy, ipPrefix, nexthop, true)
	return route != nil, err
}

// newLogicalRouterStaticRoute return logical router static route with basic information
func (c *OVNNbClient) newLogicalRouterStaticRoute(lrName, routeTable, policy, ipPrefix, nexthop string, bfdID *string, externalIDs map[string]string, options ...func(route *ovnnb.LogicalRouterStaticRoute)) (*ovnnb.LogicalRouterStaticRoute, error) {
	if len(lrName) == 0 {
		return nil, errors.New("the logical router name is required")
	}

	if len(policy) == 0 {
		policy = ovnnb.LogicalRouterStaticRoutePolicyDstIP
	}

	exists, err := c.LogicalRouterStaticRouteExists(lrName, routeTable, policy, ipPrefix, nexthop)
	if err != nil {
		klog.Error(err)
		return nil, fmt.Errorf("get logical router %s route: %w", lrName, err)
	}

	// found, ignore
	if exists {
		return nil, nil
	}

	route := &ovnnb.LogicalRouterStaticRoute{
		UUID:        ovsclient.NamedUUID(),
		Policy:      &policy,
		IPPrefix:    ipPrefix,
		Nexthop:     nexthop,
		RouteTable:  routeTable,
		ExternalIDs: externalIDs,
	}
	for _, option := range options {
		option(route)
	}

	if bfdID != nil {
		route.BFD = bfdID
		if route.Options == nil {
			route.Options = make(map[string]string)
		}
		route.Options[util.StaticRouteBfdEcmp] = "true"
	}
	return route, nil
}

func (c *OVNNbClient) listLogicalRouterStaticRoutesByFilter(lrName string, filter func(route *ovnnb.LogicalRouterStaticRoute) bool) ([]*ovnnb.LogicalRouterStaticRoute, error) {
	lr, err := c.GetLogicalRouter(lrName, false)
	if err != nil {
		klog.Error(err)
		return nil, err
	}

	routeList := make([]*ovnnb.LogicalRouterStaticRoute, 0, len(lr.StaticRoutes))
	for _, uuid := range lr.StaticRoutes {
		route, err := c.GetLogicalRouterStaticRouteByUUID(uuid)
		if err != nil {
			if errors.Is(err, client.ErrNotFound) {
				continue
			}
			klog.Error(err)
			return nil, err
		}
		if filter == nil || filter(route) {
			routeList = append(routeList, route)
		}
	}

	return routeList, nil
}

// batchListLogicalRouterStaticRoutesForDelete batch list route which match the given condition when need delete static route
func (c *OVNNbClient) batchListLogicalRouterStaticRoutesForDelete(staticRoutes map[string]string, lrStaticRoute []string) ([]*ovnnb.LogicalRouterStaticRoute, error) {
	lrStaticRouteSet := set.New(lrStaticRoute...)
	fnFilter := func(route *ovnnb.LogicalRouterStaticRoute) bool {
		if !lrStaticRouteSet.Has(route.UUID) {
			return false
		}
		key := createStaticRouteKey(route.RouteTable, *route.Policy, route.IPPrefix)
		_, exists := staticRoutes[key]
		return exists
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	defer cancel()

	routeList := make([]*ovnnb.LogicalRouterStaticRoute, 0)
	if err := c.ovsDbClient.WhereCache(fnFilter).List(ctx, &routeList); err != nil {
		klog.Error(err)
		return nil, fmt.Errorf("batch list logical staric router %v lr staric route %v route: %w", staticRoutes, lrStaticRoute, err)
	}

	return routeList, nil
}

func createStaticRouteKey(routeTable, policy, ipPrefix string) string {
	return fmt.Sprintf("%s-%s-%s", routeTable, policy, ipPrefix)
}
