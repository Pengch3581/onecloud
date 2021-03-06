// Copyright 2019 Yunion
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

package regiondrivers

import (
	"context"
	"fmt"
	"regexp"

	"yunion.io/x/jsonutils"
	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/utils"

	api "yunion.io/x/onecloud/pkg/apis/compute"
	"yunion.io/x/onecloud/pkg/cloudcommon/db"
	"yunion.io/x/onecloud/pkg/cloudcommon/db/taskman"
	"yunion.io/x/onecloud/pkg/cloudcommon/validators"
	"yunion.io/x/onecloud/pkg/cloudprovider"
	"yunion.io/x/onecloud/pkg/compute/models"
	"yunion.io/x/onecloud/pkg/compute/options"
	"yunion.io/x/onecloud/pkg/httperrors"
	"yunion.io/x/onecloud/pkg/mcclient"
	"yunion.io/x/onecloud/pkg/util/choices"
	"yunion.io/x/onecloud/pkg/util/httputils"
	"yunion.io/x/onecloud/pkg/util/rand"
)

type SKVMRegionDriver struct {
	SBaseRegionDriver
}

func init() {
	driver := SKVMRegionDriver{}
	models.RegisterRegionDriver(&driver)
}

func RunValidators(validators map[string]validators.IValidator, data *jsonutils.JSONDict, optional bool) error {
	for _, v := range validators {
		if optional {
			v.Optional(true)
		}
		if err := v.Validate(data); err != nil {
			return err
		}
	}

	return nil
}

func (self *SKVMRegionDriver) GetProvider() string {
	return api.CLOUD_PROVIDER_ONECLOUD
}

func (self *SKVMRegionDriver) ValidateCreateLoadbalancerData(ctx context.Context, userCred mcclient.TokenCredential, ownerId mcclient.IIdentityProvider, data *jsonutils.JSONDict) (*jsonutils.JSONDict, error) {
	networkV := validators.NewModelIdOrNameValidator("network", "network", ownerId)
	addressV := validators.NewIPv4AddrValidator("address")
	clusterV := validators.NewModelIdOrNameValidator("cluster", "loadbalancercluster", ownerId)
	keyV := map[string]validators.IValidator{
		"status":  validators.NewStringChoicesValidator("status", api.LB_STATUS_SPEC).Default(api.LB_STATUS_ENABLED),
		"address": addressV.Optional(true),
		"network": networkV,
		"cluster": clusterV.Optional(true),
	}
	if err := RunValidators(keyV, data, false); err != nil {
		return nil, err
	}

	network := networkV.Model.(*models.SNetwork)
	region, zone, vpc, _, err := network.ValidateElbNetwork(addressV.IP)
	if err != nil {
		return nil, err
	}

	if zone == nil {
		return nil, httperrors.NewInputParameterError("zone info missing")
	}

	if clusterV.Model == nil {
		clusters := models.LoadbalancerClusterManager.FindByZoneId(zone.Id)
		if len(clusters) == 0 {
			return nil, httperrors.NewInputParameterError("zone %s(%s) has no lbcluster", zone.Name, zone.Id)
		}
		var (
			wireMatched []*models.SLoadbalancerCluster
			wireNeutral []*models.SLoadbalancerCluster
		)
		for i := range clusters {
			c := &clusters[i]
			if c.WireId != "" {
				if c.WireId == network.WireId {
					wireMatched = append(wireMatched, c)
				}
			} else {
				wireNeutral = append(wireNeutral, c)
			}
		}
		var choices []*models.SLoadbalancerCluster
		if len(wireMatched) > 0 {
			choices = wireMatched
		} else if len(wireNeutral) > 0 {
			choices = wireNeutral
		} else {
			return nil, httperrors.NewInputParameterError("no viable lbcluster")
		}
		i := rand.Intn(len(choices))
		data.Set("cluster_id", jsonutils.NewString(choices[i].Id))
	} else {
		cluster := clusterV.Model.(*models.SLoadbalancerCluster)
		if cluster.ZoneId != zone.Id {
			return nil, httperrors.NewInputParameterError("cluster zone %s does not match network zone %s ",
				cluster.ZoneId, zone.Id)
		}
		if cluster.WireId != "" && cluster.WireId != network.WireId {
			return nil, httperrors.NewInputParameterError("cluster wire affiliation does not match network's: %s != %s",
				cluster.WireId, network.WireId)
		}
	}

	data.Set("cloudregion_id", jsonutils.NewString(region.GetId()))
	data.Set("zone_id", jsonutils.NewString(zone.GetId()))
	data.Set("vpc_id", jsonutils.NewString(vpc.GetId()))
	data.Set("network_type", jsonutils.NewString(api.LB_NETWORK_TYPE_CLASSIC))
	data.Set("address_type", jsonutils.NewString(api.LB_ADDR_TYPE_INTRANET))
	return data, nil
}

func (self *SKVMRegionDriver) ValidateCreateLoadbalancerAclData(ctx context.Context, userCred mcclient.TokenCredential, data *jsonutils.JSONDict) (*jsonutils.JSONDict, error) {
	return data, nil
}

func (self *SKVMRegionDriver) ValidateCreateLoadbalancerCertificateData(ctx context.Context, userCred mcclient.TokenCredential, data *jsonutils.JSONDict) (*jsonutils.JSONDict, error) {
	return data, nil
}

func (self *SKVMRegionDriver) ValidateUpdateLoadbalancerCertificateData(ctx context.Context, userCred mcclient.TokenCredential, data *jsonutils.JSONDict) (*jsonutils.JSONDict, error) {
	return data, nil
}

func (self *SKVMRegionDriver) ValidateCreateLoadbalancerBackendGroupData(ctx context.Context, userCred mcclient.TokenCredential, data *jsonutils.JSONDict, lb *models.SLoadbalancer, backends []cloudprovider.SLoadbalancerBackend) (*jsonutils.JSONDict, error) {
	for _, backend := range backends {
		switch backend.BackendType {
		case api.LB_BACKEND_GUEST:
			if backend.ZoneId != lb.ZoneId {
				return nil, fmt.Errorf("zone of host %q (%s) != zone of loadbalancer %q (%s)",
					backend.HostName, backend.ZoneId, lb.Name, lb.ZoneId)
			}
		}
	}
	return data, nil
}

func (self *SKVMRegionDriver) ValidateCreateLoadbalancerBackendData(ctx context.Context, userCred mcclient.TokenCredential, data *jsonutils.JSONDict, backendType string, lb *models.SLoadbalancer, backendGroup *models.SLoadbalancerBackendGroup, backend db.IModel) (*jsonutils.JSONDict, error) {
	ownerId := ctx.Value("ownerId").(mcclient.IIdentityProvider)
	man := models.LoadbalancerBackendManager
	backendTypeV := validators.NewStringChoicesValidator("backend_type", api.LB_BACKEND_TYPES)
	keyV := map[string]validators.IValidator{
		"backend_type": backendTypeV,
		"weight":       validators.NewRangeValidator("weight", 1, 256).Default(1),
		"port":         validators.NewPortValidator("port"),
		"send_proxy":   validators.NewStringChoicesValidator("send_proxy", api.LB_SENDPROXY_CHOICES).Default(api.LB_SENDPROXY_OFF),
		"ssl":          validators.NewStringChoicesValidator("ssl", api.LB_BOOL_VALUES).Default(api.LB_BOOL_OFF),
	}

	if err := RunValidators(keyV, data, false); err != nil {
		return nil, err
	}

	var basename string
	switch backendType {
	case api.LB_BACKEND_GUEST:
		backendV := validators.NewModelIdOrNameValidator("backend", "server", ownerId)
		err := backendV.Validate(data)
		if err != nil {
			return nil, err
		}
		guest := backendV.Model.(*models.SGuest)
		err = man.ValidateBackendVpc(lb, guest, backendGroup)
		if err != nil {
			return nil, err
		}
		basename = guest.Name
		backend = backendV.Model
	case api.LB_BACKEND_HOST:
		if !db.IsAdminAllowCreate(userCred, man) {
			return nil, fmt.Errorf("only sysadmin can specify host as backend")
		}
		backendV := validators.NewModelIdOrNameValidator("backend", "host", userCred)
		err := backendV.Validate(data)
		if err != nil {
			return nil, err
		}
		host := backendV.Model.(*models.SHost)
		{
			if len(host.AccessIp) == 0 {
				return nil, fmt.Errorf("host %s has no access ip", host.GetId())
			}
			data.Set("address", jsonutils.NewString(host.AccessIp))
		}
		basename = host.Name
		backend = backendV.Model
	case api.LB_BACKEND_IP:
		if !db.IsAdminAllowCreate(userCred, man) {
			return nil, fmt.Errorf("only sysadmin can specify ip address as backend")
		}
		backendV := validators.NewIPv4AddrValidator("backend")
		err := backendV.Validate(data)
		if err != nil {
			return nil, err
		}
		ip := backendV.IP.String()
		data.Set("address", jsonutils.NewString(ip))
		basename = ip
	default:
		return nil, fmt.Errorf("internal error: unexpected backend type %s", backendType)
	}

	name, _ := data.GetString("name")
	if name == "" {
		name = fmt.Sprintf("%s-%s-%s-%s", backendGroup.Name, backendType, basename, rand.String(4))
	}

	switch backendType {
	case api.LB_BACKEND_GUEST:
		guest := backend.(*models.SGuest)
		{
			// guest zone must match that of loadbalancer's
			host := guest.GetHost()
			if host == nil {
				return nil, fmt.Errorf("error getting host of guest %s", guest.GetId())
			}

			if lb == nil {
				return nil, fmt.Errorf("error loadbalancer of backend group %s", backendGroup.GetId())
			}
			if host.ZoneId != lb.ZoneId {
				return nil, fmt.Errorf("zone of host %q (%s) != zone of loadbalancer %q (%s)",
					host.Name, host.ZoneId, lb.Name, lb.ZoneId)
			}
		}
		{
			// get guest intranet address
			//
			// NOTE add address hint (cidr) if needed
			address, err := models.LoadbalancerBackendManager.GetGuestAddress(guest)
			if err != nil {
				return nil, err
			}
			data.Set("address", jsonutils.NewString(address))
		}
	}

	data.Set("name", jsonutils.NewString(name))
	data.Set("manager_id", jsonutils.NewString(lb.GetCloudproviderId()))
	data.Set("cloudregion_id", jsonutils.NewString(lb.GetRegionId()))
	return data, nil
}

func (self *SKVMRegionDriver) ValidateUpdateLoadbalancerBackendData(ctx context.Context, userCred mcclient.TokenCredential, data *jsonutils.JSONDict, lbbg *models.SLoadbalancerBackendGroup) (*jsonutils.JSONDict, error) {
	keyV := map[string]validators.IValidator{
		"weight":     validators.NewRangeValidator("weight", 1, 256),
		"port":       validators.NewPortValidator("port"),
		"send_proxy": validators.NewStringChoicesValidator("send_proxy", api.LB_SENDPROXY_CHOICES),
		"ssl":        validators.NewStringChoicesValidator("ssl", api.LB_BOOL_VALUES),
	}

	if err := RunValidators(keyV, data, true); err != nil {
		return nil, err
	}

	return data, nil
}

func (self *SKVMRegionDriver) ValidateCreateLoadbalancerListenerRuleData(ctx context.Context, userCred mcclient.TokenCredential, ownerId mcclient.IIdentityProvider, data *jsonutils.JSONDict, backendGroup db.IModel) (*jsonutils.JSONDict, error) {
	listenerV := validators.NewModelIdOrNameValidator("listener", "loadbalancerlistener", ownerId)
	domainV := validators.NewHostPortValidator("domain").OptionalPort(true)
	pathV := validators.NewURLPathValidator("path")
	redirectV := validators.NewStringChoicesValidator("redirect", api.LB_REDIRECT_TYPES)
	keyV := map[string]validators.IValidator{
		"status": validators.NewStringChoicesValidator("status", api.LB_STATUS_SPEC).Default(api.LB_STATUS_ENABLED),

		"listener": listenerV,
		"domain":   domainV.AllowEmpty(true).Default(""),
		"path":     pathV.Default(""),

		"http_request_rate":         validators.NewNonNegativeValidator("http_request_rate").Default(0),
		"http_request_rate_per_src": validators.NewNonNegativeValidator("http_request_rate_per_src").Default(0),

		"redirect":        redirectV.Default(api.LB_REDIRECT_OFF),
		"redirect_code":   validators.NewIntChoicesValidator("redirect_code", api.LB_REDIRECT_CODES).Default(api.LB_REDIRECT_CODE_302),
		"redirect_scheme": validators.NewStringChoicesValidator("redirect_scheme", api.LB_REDIRECT_SCHEMES).Optional(true),
		"redirect_host":   validators.NewHostPortValidator("redirect_host").OptionalPort(true).Optional(true),
		"redirect_path":   validators.NewURLPathValidator("redirect_path").Optional(true),
	}

	if err := RunValidators(keyV, data, false); err != nil {
		return nil, err
	}

	listener := listenerV.Model.(*models.SLoadbalancerListener)
	listenerType := listener.ListenerType
	if listenerType != api.LB_LISTENER_TYPE_HTTP && listenerType != api.LB_LISTENER_TYPE_HTTPS {
		return nil, httperrors.NewInputParameterError("listener type must be http/https, got %s", listenerType)
	}
	if listener.Redirect != api.LB_REDIRECT_OFF {
		return nil, httperrors.NewInputParameterError("do not allow adding rules for redirect listener")
	}

	{
		if redirectV.Value == api.LB_REDIRECT_OFF {
			if backendGroup == nil {
				return nil, httperrors.NewInputParameterError("backend_group argument is missing")
			}
		}
		if lbbg, ok := backendGroup.(*models.SLoadbalancerBackendGroup); ok && lbbg.LoadbalancerId != listener.LoadbalancerId {
			return nil, httperrors.NewInputParameterError("backend group %s(%s) belongs to loadbalancer %s instead of %s",
				lbbg.Name, lbbg.Id, lbbg.LoadbalancerId, listener.LoadbalancerId)
		}
	}

	err := models.LoadbalancerListenerRuleCheckUniqueness(ctx, listener, domainV.Value, pathV.Value)
	if err != nil {
		return nil, err
	}

	data.Set("cloudregion_id", jsonutils.NewString(listener.GetRegionId()))
	data.Set("manager_id", jsonutils.NewString(listener.GetCloudproviderId()))
	return data, nil
}

func (self *SKVMRegionDriver) ValidateUpdateLoadbalancerListenerRuleData(ctx context.Context, userCred mcclient.TokenCredential, data *jsonutils.JSONDict, backendGroup db.IModel) (*jsonutils.JSONDict, error) {
	lbr := ctx.Value("lbr").(*models.SLoadbalancerListenerRule)
	domainV := validators.NewHostPortValidator("domain").OptionalPort(true)
	pathV := validators.NewURLPathValidator("path")
	redirectV := validators.NewStringChoicesValidator("redirect", api.LB_REDIRECT_TYPES)
	redirectV.Default(lbr.Redirect)
	keyV := map[string]validators.IValidator{
		"domain": domainV.AllowEmpty(true).Default(lbr.Domain),
		"path":   pathV.Default(lbr.Path),

		"http_request_rate":         validators.NewNonNegativeValidator("http_request_rate"),
		"http_request_rate_per_src": validators.NewNonNegativeValidator("http_request_rate_per_src"),

		"redirect":        redirectV,
		"redirect_code":   validators.NewIntChoicesValidator("redirect_code", api.LB_REDIRECT_CODES),
		"redirect_scheme": validators.NewStringChoicesValidator("redirect_scheme", api.LB_REDIRECT_SCHEMES),
		"redirect_host":   validators.NewHostPortValidator("redirect_host").OptionalPort(true),
		"redirect_path":   validators.NewURLPathValidator("redirect_path"),
	}
	for _, v := range keyV {
		v.Optional(true)
		if err := v.Validate(data); err != nil {
			return nil, err
		}
	}
	if lbr.Redirect != redirectV.Value {
		// this can be relaxed to do not allow on/off
		return nil, httperrors.NewInputParameterError("do not allow changing redirect type")
	}

	if backendGroup, ok := backendGroup.(*models.SLoadbalancerBackendGroup); ok && backendGroup.Id != lbr.BackendGroupId {
		listenerM, err := models.LoadbalancerListenerManager.FetchById(lbr.ListenerId)
		if err != nil {
			return nil, httperrors.NewInputParameterError("loadbalancerlistenerrule %s(%s): fetching listener %s failed",
				lbr.Name, lbr.Id, lbr.ListenerId)
		}
		listener := listenerM.(*models.SLoadbalancerListener)
		if backendGroup.LoadbalancerId != listener.LoadbalancerId {
			return nil, httperrors.NewInputParameterError("backend group %s(%s) belongs to loadbalancer %s instead of %s",
				backendGroup.Name, backendGroup.Id, backendGroup.LoadbalancerId, listener.LoadbalancerId)
		}
	}
	return data, nil
}

func (self *SKVMRegionDriver) ValidateCreateLoadbalancerListenerData(ctx context.Context, userCred mcclient.TokenCredential, ownerId mcclient.IIdentityProvider, data *jsonutils.JSONDict, lb *models.SLoadbalancer, backendGroup db.IModel) (*jsonutils.JSONDict, error) {
	listenerTypeV := validators.NewStringChoicesValidator("listener_type", api.LB_LISTENER_TYPES)
	listenerPortV := validators.NewPortValidator("listener_port")
	aclStatusV := validators.NewStringChoicesValidator("acl_status", api.LB_BOOL_VALUES)
	aclTypeV := validators.NewStringChoicesValidator("acl_type", api.LB_ACL_TYPES)
	aclV := validators.NewModelIdOrNameValidator("acl", "loadbalanceracl", ownerId)
	redirectV := validators.NewStringChoicesValidator("redirect", api.LB_REDIRECT_TYPES)
	keyV := map[string]validators.IValidator{
		"status": validators.NewStringChoicesValidator("status", api.LB_STATUS_SPEC).Default(api.LB_STATUS_ENABLED),

		"listener_type": listenerTypeV,
		"listener_port": listenerPortV,

		"send_proxy": validators.NewStringChoicesValidator("send_proxy", api.LB_SENDPROXY_CHOICES).Default(api.LB_SENDPROXY_OFF),

		"acl_status": aclStatusV.Default(api.LB_BOOL_OFF),
		"acl_type":   aclTypeV.Optional(true),
		"acl":        aclV.Optional(true),

		"scheduler":   validators.NewStringChoicesValidator("scheduler", api.LB_SCHEDULER_TYPES),
		"egress_mbps": validators.NewRangeValidator("egress_mbps", api.LB_MbpsMin, api.LB_MbpsMax).Optional(true),

		"client_request_timeout":  validators.NewRangeValidator("client_request_timeout", 0, 600).Default(10),
		"client_idle_timeout":     validators.NewRangeValidator("client_idle_timeout", 0, 600).Default(90),
		"backend_connect_timeout": validators.NewRangeValidator("backend_connect_timeout", 0, 180).Default(5),
		"backend_idle_timeout":    validators.NewRangeValidator("backend_idle_timeout", 0, 600).Default(90),

		"sticky_session":                validators.NewStringChoicesValidator("sticky_session", api.LB_BOOL_VALUES).Default(api.LB_BOOL_OFF),
		"sticky_session_type":           validators.NewStringChoicesValidator("sticky_session_type", api.LB_STICKY_SESSION_TYPES).Default(api.LB_STICKY_SESSION_TYPE_INSERT),
		"sticky_session_cookie":         validators.NewRegexpValidator("sticky_session_cookie", regexp.MustCompile(`\w+`)).Optional(true),
		"sticky_session_cookie_timeout": validators.NewNonNegativeValidator("sticky_session_cookie_timeout").Optional(true),

		"x_forwarded_for": validators.NewBoolValidator("x_forwarded_for").Default(true),
		"gzip":            validators.NewBoolValidator("gzip").Default(false),

		"http_request_rate":         validators.NewNonNegativeValidator("http_request_rate").Default(0),
		"http_request_rate_per_src": validators.NewNonNegativeValidator("http_request_rate_per_src").Default(0),

		"redirect":        redirectV.Default(api.LB_REDIRECT_OFF),
		"redirect_code":   validators.NewIntChoicesValidator("redirect_code", api.LB_REDIRECT_CODES).Default(api.LB_REDIRECT_CODE_302),
		"redirect_scheme": validators.NewStringChoicesValidator("redirect_scheme", api.LB_REDIRECT_SCHEMES).Optional(true),
		"redirect_host":   validators.NewHostPortValidator("redirect_host").OptionalPort(true).Optional(true),
		"redirect_path":   validators.NewURLPathValidator("redirect_path").Optional(true),
	}

	if err := RunValidators(keyV, data, false); err != nil {
		return nil, err
	}

	//  listener uniqueness
	listenerType := listenerTypeV.Value
	err := models.LoadbalancerListenerManager.CheckListenerUniqueness(ctx, lb, listenerType, listenerPortV.Value)
	if err != nil {
		return nil, err
	}

	redirectType := redirectV.Value
	if redirectType != api.LB_REDIRECT_OFF {
		if listenerType != api.LB_LISTENER_TYPE_HTTP && listenerType != api.LB_LISTENER_TYPE_HTTPS {
			return nil, httperrors.NewInputParameterError("redirect can only be enabled for http/https listener")
		}
	}

	// backendgroup check
	if lbbg, ok := backendGroup.(*models.SLoadbalancerBackendGroup); ok && lbbg.LoadbalancerId != lb.Id {
		return nil, httperrors.NewInputParameterError("backend group %s(%s) belongs to loadbalancer %s instead of %s",
			lbbg.Name, lbbg.Id, lbbg.LoadbalancerId, lb.Id)
	}

	// https additional certificate check
	if listenerType == api.LB_LISTENER_TYPE_HTTPS {
		certV := validators.NewModelIdOrNameValidator("certificate", "loadbalancercertificate", ownerId)
		tlsCipherPolicyV := validators.NewStringChoicesValidator("tls_cipher_policy", api.LB_TLS_CIPHER_POLICIES).Default(api.LB_TLS_CIPHER_POLICY_1_2)
		httpsV := map[string]validators.IValidator{
			"certificate":       certV,
			"tls_cipher_policy": tlsCipherPolicyV,
			"enable_http2":      validators.NewBoolValidator("enable_http2").Default(true),
		}

		if err := RunValidators(httpsV, data, false); err != nil {
			return nil, err
		}
	}

	// health check default depends on input parameters
	checkTypeV := models.LoadbalancerListenerManager.CheckTypeV(listenerType)
	keyVHealth := map[string]validators.IValidator{
		"health_check":      validators.NewStringChoicesValidator("health_check", api.LB_BOOL_VALUES).Default(api.LB_BOOL_ON),
		"health_check_type": checkTypeV,

		"health_check_domain":    validators.NewDomainNameValidator("health_check_domain").AllowEmpty(true).Default(""),
		"health_check_path":      validators.NewURLPathValidator("health_check_path").Default(""),
		"health_check_http_code": validators.NewStringMultiChoicesValidator("health_check_http_code", api.LB_HEALTH_CHECK_HTTP_CODES).Sep(",").Default(api.LB_HEALTH_CHECK_HTTP_CODE_DEFAULT),

		"health_check_rise":     validators.NewRangeValidator("health_check_rise", 1, 1000).Default(3),
		"health_check_fall":     validators.NewRangeValidator("health_check_fall", 1, 1000).Default(3),
		"health_check_timeout":  validators.NewRangeValidator("health_check_timeout", 1, 300).Default(5),
		"health_check_interval": validators.NewRangeValidator("health_check_interval", 1, 1000).Default(5),
	}

	if err := RunValidators(keyVHealth, data, false); err != nil {
		return nil, err
	}

	// acl check
	if err := models.LoadbalancerListenerManager.ValidateAcl(aclStatusV, aclTypeV, aclV, data, api.CLOUD_PROVIDER_ONECLOUD); err != nil {
		return nil, err
	}

	data.Set("manager_id", jsonutils.NewString(lb.GetCloudproviderId()))
	data.Set("cloudregion_id", jsonutils.NewString(lb.GetRegionId()))
	return data, nil
}

func (self *SKVMRegionDriver) ValidateUpdateLoadbalancerListenerData(ctx context.Context, userCred mcclient.TokenCredential, data *jsonutils.JSONDict, lblis *models.SLoadbalancerListener, backendGroup db.IModel) (*jsonutils.JSONDict, error) {
	ownerId := lblis.GetOwnerId()
	aclStatusV := validators.NewStringChoicesValidator("acl_status", api.LB_BOOL_VALUES)
	aclStatusV.Default(lblis.AclStatus)
	aclTypeV := validators.NewStringChoicesValidator("acl_type", api.LB_ACL_TYPES)
	if api.LB_ACL_TYPES.Has(lblis.AclType) {
		aclTypeV.Default(lblis.AclType)
	}

	aclV := validators.NewModelIdOrNameValidator("acl", "loadbalanceracl", ownerId)
	if len(lblis.AclId) > 0 {
		aclV.Default(lblis.AclId)
	}

	redirectV := validators.NewStringChoicesValidator("redirect", api.LB_REDIRECT_TYPES)
	redirectV.Default(lblis.Redirect)

	certV := validators.NewModelIdOrNameValidator("certificate", "loadbalancercertificate", ownerId)
	tlsCipherPolicyV := validators.NewStringChoicesValidator("tls_cipher_policy", api.LB_TLS_CIPHER_POLICIES).Default(api.LB_TLS_CIPHER_POLICY_1_2)
	keyV := map[string]validators.IValidator{
		"send_proxy": validators.NewStringChoicesValidator("send_proxy", api.LB_SENDPROXY_CHOICES),

		"acl_status": aclStatusV,
		"acl_type":   aclTypeV,
		"acl":        aclV,

		"scheduler":   validators.NewStringChoicesValidator("scheduler", api.LB_SCHEDULER_TYPES),
		"egress_mbps": validators.NewRangeValidator("egress_mbps", api.LB_MbpsMin, api.LB_MbpsMax),

		"client_request_timeout":  validators.NewRangeValidator("client_request_timeout", 0, 600),
		"client_idle_timeout":     validators.NewRangeValidator("client_idle_timeout", 0, 600),
		"backend_connect_timeout": validators.NewRangeValidator("backend_connect_timeout", 0, 180),
		"backend_idle_timeout":    validators.NewRangeValidator("backend_idle_timeout", 0, 600),

		"sticky_session":                validators.NewStringChoicesValidator("sticky_session", api.LB_BOOL_VALUES),
		"sticky_session_type":           validators.NewStringChoicesValidator("sticky_session_type", api.LB_STICKY_SESSION_TYPES),
		"sticky_session_cookie":         validators.NewRegexpValidator("sticky_session_cookie", regexp.MustCompile(`\w+`)),
		"sticky_session_cookie_timeout": validators.NewNonNegativeValidator("sticky_session_cookie_timeout"),

		"health_check":      validators.NewStringChoicesValidator("health_check", api.LB_BOOL_VALUES),
		"health_check_type": models.LoadbalancerListenerManager.CheckTypeV(lblis.ListenerType),

		"health_check_domain":    validators.NewDomainNameValidator("health_check_domain").AllowEmpty(true),
		"health_check_path":      validators.NewURLPathValidator("health_check_path"),
		"health_check_http_code": validators.NewStringMultiChoicesValidator("health_check_http_code", api.LB_HEALTH_CHECK_HTTP_CODES).Sep(","),

		"health_check_rise":     validators.NewRangeValidator("health_check_rise", 1, 1000),
		"health_check_fall":     validators.NewRangeValidator("health_check_fall", 1, 1000),
		"health_check_timeout":  validators.NewRangeValidator("health_check_timeout", 1, 300),
		"health_check_interval": validators.NewRangeValidator("health_check_interval", 1, 1000),

		"x_forwarded_for": validators.NewBoolValidator("x_forwarded_for"),
		"gzip":            validators.NewBoolValidator("gzip"),

		"http_request_rate":         validators.NewNonNegativeValidator("http_request_rate"),
		"http_request_rate_per_src": validators.NewNonNegativeValidator("http_request_rate_per_src"),

		"certificate":       certV,
		"tls_cipher_policy": tlsCipherPolicyV,
		"enable_http2":      validators.NewBoolValidator("enable_http2"),

		"redirect":        redirectV,
		"redirect_code":   validators.NewIntChoicesValidator("redirect_code", api.LB_REDIRECT_CODES),
		"redirect_scheme": validators.NewStringChoicesValidator("redirect_scheme", api.LB_REDIRECT_SCHEMES),
		"redirect_host":   validators.NewHostPortValidator("redirect_host").OptionalPort(true),
		"redirect_path":   validators.NewURLPathValidator("redirect_path"),
	}

	if err := RunValidators(keyV, data, true); err != nil {
		return nil, err
	}

	if lblis.Redirect != redirectV.Value {
		// this can be relaxed to do not allow on/off
		return nil, httperrors.NewInputParameterError("do not allow changing redirect type")
	}

	if err := models.LoadbalancerListenerManager.ValidateAcl(aclStatusV, aclTypeV, aclV, data, lblis.GetProviderName()); err != nil {
		return nil, err
	}

	{
		if backendGroup == nil {
			if lblis.ListenerType != api.LB_LISTENER_TYPE_HTTP &&
				lblis.ListenerType != api.LB_LISTENER_TYPE_HTTPS {
				return nil, httperrors.NewInputParameterError("non http listener must have backend group set")
			}
		} else if lbbg, ok := backendGroup.(*models.SLoadbalancerBackendGroup); ok && lbbg.LoadbalancerId != lblis.LoadbalancerId {
			return nil, httperrors.NewInputParameterError("backend group %s(%s) belongs to loadbalancer %s instead of %s",
				lbbg.Name, lbbg.Id, lbbg.LoadbalancerId, lblis.LoadbalancerId)
		}
	}

	return data, nil
}

func (self *SKVMRegionDriver) RequestCreateLoadbalancer(ctx context.Context, userCred mcclient.TokenCredential, lb *models.SLoadbalancer, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		_, err := db.Update(lb, func() error {
			if lb.AddressType == api.LB_ADDR_TYPE_INTRANET {
				// TODO support use reserved ip address
				// TODO prefer ip address from server_type loadbalancer?
				req := &models.SLoadbalancerNetworkRequestData{
					Loadbalancer: lb,
					NetworkId:    lb.NetworkId,
					Address:      lb.Address,
				}
				// NOTE the small window when agents can see the ephemeral address
				ln, err := models.LoadbalancernetworkManager.NewLoadbalancerNetwork(ctx, userCred, req)
				if err != nil {
					log.Errorf("allocating loadbalancer network failed: %v, req: %#v", err, req)
					lb.Address = ""
				} else {
					lb.Address = ln.IpAddr
				}
			}
			return nil
		})
		return nil, err
	})
	return nil
}

func (self *SKVMRegionDriver) RequestStartLoadbalancer(ctx context.Context, userCred mcclient.TokenCredential, lb *models.SLoadbalancer, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) RequestStopLoadbalancer(ctx context.Context, userCred mcclient.TokenCredential, lb *models.SLoadbalancer, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) RequestSyncstatusLoadbalancer(ctx context.Context, userCred mcclient.TokenCredential, lb *models.SLoadbalancer, task taskman.ITask) error {
	originStatus, _ := task.GetParams().GetString("origin_status")
	if utils.IsInStringArray(originStatus, []string{api.LB_STATUS_ENABLED, api.LB_STATUS_DISABLED}) {
		lb.SetStatus(userCred, originStatus, "")
	} else {
		lb.SetStatus(userCred, api.LB_STATUS_ENABLED, "")
	}
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) RequestDeleteLoadbalancer(ctx context.Context, userCred mcclient.TokenCredential, lb *models.SLoadbalancer, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) RequestCreateLoadbalancerAcl(ctx context.Context, userCred mcclient.TokenCredential, lbacl *models.SCachedLoadbalancerAcl, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) RequestSyncLoadbalancerAcl(ctx context.Context, userCred mcclient.TokenCredential, lbacl *models.SCachedLoadbalancerAcl, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) RequestDeleteLoadbalancerAcl(ctx context.Context, userCred mcclient.TokenCredential, lbacl *models.SCachedLoadbalancerAcl, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) RequestSyncLoadbalancerBackendGroup(ctx context.Context, userCred mcclient.TokenCredential, lblis *models.SLoadbalancerListener, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) RequestPullRegionLoadbalancerBackendGroup(ctx context.Context, userCred mcclient.TokenCredential, syncResults models.SSyncResultSet, provider *models.SCloudprovider, localRegion *models.SCloudregion, remoteRegion cloudprovider.ICloudRegion, syncRange *models.SSyncRange) error {
	return nil
}

func (self *SKVMRegionDriver) RequestPullLoadbalancerBackendGroup(ctx context.Context, userCred mcclient.TokenCredential, syncResults models.SSyncResultSet, provider *models.SCloudprovider, localLoadbalancer *models.SLoadbalancer, remoteLoadbalancer cloudprovider.ICloudLoadbalancer, syncRange *models.SSyncRange) error {
	return nil
}

func (self *SKVMRegionDriver) RequestCreateLoadbalancerCertificate(ctx context.Context, userCred mcclient.TokenCredential, lbcert *models.SCachedLoadbalancerCertificate, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) RequestDeleteLoadbalancerCertificate(ctx context.Context, userCred mcclient.TokenCredential, lbcert *models.SCachedLoadbalancerCertificate, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) RequestCreateLoadbalancerBackendGroup(ctx context.Context, userCred mcclient.TokenCredential, lbbg *models.SLoadbalancerBackendGroup, backends []cloudprovider.SLoadbalancerBackend, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		for _, backend := range backends {
			loadbalancerBackend := models.SLoadbalancerBackend{
				BackendId:   backend.ID,
				BackendType: backend.BackendType,
				BackendRole: backend.BackendRole,
				Weight:      backend.Weight,
				Address:     backend.Address,
				Port:        backend.Port,
			}
			loadbalancerBackend.BackendGroupId = lbbg.Id
			loadbalancerBackend.Status = api.LB_STATUS_ENABLED
			loadbalancerBackend.ProjectId = userCred.GetProjectId()
			loadbalancerBackend.DomainId = userCred.GetProjectDomainId()
			loadbalancerBackend.Name = fmt.Sprintf("%s-%s-%s", lbbg.Name, backend.BackendType, backend.Name)
			if err := models.LoadbalancerBackendManager.TableSpec().Insert(&loadbalancerBackend); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	return nil
}

func (self *SKVMRegionDriver) RequestDeleteLoadbalancerBackendGroup(ctx context.Context, userCred mcclient.TokenCredential, lbbg *models.SLoadbalancerBackendGroup, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) RequestCreateLoadbalancerBackend(ctx context.Context, userCred mcclient.TokenCredential, lbb *models.SLoadbalancerBackend, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) ValidateDeleteLoadbalancerCondition(ctx context.Context, lb *models.SLoadbalancer) error {
	return nil
}

func (self *SKVMRegionDriver) ValidateDeleteLoadbalancerBackendCondition(ctx context.Context, lbb *models.SLoadbalancerBackend) error {
	return nil
}

func (self *SKVMRegionDriver) ValidateDeleteLoadbalancerBackendGroupCondition(ctx context.Context, lbbg *models.SLoadbalancerBackendGroup) error {
	return nil
}

func (self *SKVMRegionDriver) GetBackendStatusForAdd() []string {
	return []string{api.VM_RUNNING, api.VM_READY}
}

func (self *SKVMRegionDriver) RequestDeleteLoadbalancerBackend(ctx context.Context, userCred mcclient.TokenCredential, lbb *models.SLoadbalancerBackend, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) RequestSyncLoadbalancerBackend(ctx context.Context, userCred mcclient.TokenCredential, lbb *models.SLoadbalancerBackend, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) RequestCreateLoadbalancerListener(ctx context.Context, userCred mcclient.TokenCredential, lblis *models.SLoadbalancerListener, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) RequestDeleteLoadbalancerListener(ctx context.Context, userCred mcclient.TokenCredential, lblis *models.SLoadbalancerListener, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) RequestStartLoadbalancerListener(ctx context.Context, userCred mcclient.TokenCredential, lblis *models.SLoadbalancerListener, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) RequestStopLoadbalancerListener(ctx context.Context, userCred mcclient.TokenCredential, lblis *models.SLoadbalancerListener, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) RequestSyncstatusLoadbalancerListener(ctx context.Context, userCred mcclient.TokenCredential, lblis *models.SLoadbalancerListener, task taskman.ITask) error {
	originStatus, _ := task.GetParams().GetString("origin_status")
	if utils.IsInStringArray(originStatus, []string{api.LB_STATUS_ENABLED, api.LB_STATUS_DISABLED}) {
		lblis.SetStatus(userCred, originStatus, "")
	} else {
		lblis.SetStatus(userCred, api.LB_STATUS_ENABLED, "")
	}
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) RequestSyncLoadbalancerListener(ctx context.Context, userCred mcclient.TokenCredential, lblis *models.SLoadbalancerListener, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) RequestCreateLoadbalancerListenerRule(ctx context.Context, userCred mcclient.TokenCredential, lbr *models.SLoadbalancerListenerRule, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) RequestDeleteLoadbalancerListenerRule(ctx context.Context, userCred mcclient.TokenCredential, lbr *models.SLoadbalancerListenerRule, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) ValidateCreateVpcData(ctx context.Context, userCred mcclient.TokenCredential, input api.VpcCreateInput) (api.VpcCreateInput, error) {
	cidrChoices := choices.NewChoices("192.168.0.0/16", "10.0.0.0/8", "172.16.0.0/12")
	cidrV := validators.NewStringChoicesValidator("cidr_block", cidrChoices)
	if err := cidrV.Validate(jsonutils.Marshal(input).(*jsonutils.JSONDict)); err != nil {
		return input, err
	}
	return input, nil
}

func (self *SKVMRegionDriver) RequestDeleteVpc(ctx context.Context, userCred mcclient.TokenCredential, region *models.SCloudregion, vpc *models.SVpc, task taskman.ITask) error {
	task.ScheduleRun(nil)
	return nil
}

func (self *SKVMRegionDriver) ValidateCreateEipData(ctx context.Context, userCred mcclient.TokenCredential, input *api.SElasticipCreateInput) error {
	return httperrors.NewNotImplementedError("Not Implement EIP")
}

func (self *SKVMRegionDriver) ValidateSnapshotDelete(ctx context.Context, snapshot *models.SSnapshot) error {
	storage := snapshot.GetStorage()
	if storage == nil {
		return httperrors.NewInternalServerError("Kvm snapshot missing storage ??")
	}
	return models.GetStorageDriver(storage.StorageType).ValidateSnapshotDelete(ctx, snapshot)
}

func (self *SKVMRegionDriver) RequestDeleteSnapshot(ctx context.Context, snapshot *models.SSnapshot, task taskman.ITask) error {
	storage := snapshot.GetStorage()
	if storage == nil {
		return httperrors.NewInternalServerError("Kvm snapshot missing storage ??")
	}
	return models.GetStorageDriver(storage.StorageType).RequestDeleteSnapshot(ctx, snapshot, task)
}

func (self *SKVMRegionDriver) ValidateCreateSnapshotData(ctx context.Context, userCred mcclient.TokenCredential, disk *models.SDisk, storage *models.SStorage, input *api.SnapshotCreateInput) error {
	host := storage.GetMasterHost()
	if host == nil {
		return fmt.Errorf("failed to get master host, maybe the host is offline")
	}
	return models.GetStorageDriver(storage.StorageType).ValidateCreateSnapshotData(ctx, userCred, disk, input)
}

func (self *SKVMRegionDriver) RequestCreateSnapshot(ctx context.Context, snapshot *models.SSnapshot, task taskman.ITask) error {
	storage := snapshot.GetStorage()
	if storage == nil {
		return httperrors.NewInternalServerError("Kvm snapshot missing storage ??")
	}
	return models.GetStorageDriver(storage.StorageType).RequestCreateSnapshot(ctx, snapshot, task)
}

func (self *SKVMRegionDriver) SnapshotIsOutOfChain(disk *models.SDisk) bool {
	storage := disk.GetStorage()
	return models.GetStorageDriver(storage.StorageType).SnapshotIsOutOfChain(disk)
}

func (self *SKVMRegionDriver) GetDiskResetParams(snapshot *models.SSnapshot) *jsonutils.JSONDict {
	params := jsonutils.NewDict()
	params.Set("snapshot_id", jsonutils.NewString(snapshot.Id))
	params.Set("out_of_chain", jsonutils.NewBool(snapshot.OutOfChain))
	params.Set("location", jsonutils.NewString(snapshot.Location))
	return params
}

func (self *SKVMRegionDriver) OnDiskReset(ctx context.Context, userCred mcclient.TokenCredential, disk *models.SDisk, snapshot *models.SSnapshot, data jsonutils.JSONObject) error {
	if disk.DiskSize != snapshot.Size {
		_, err := db.Update(disk, func() error {
			disk.DiskSize = snapshot.Size
			return nil
		})
		if err != nil {
			return err
		}
	}
	storage := disk.GetStorage()
	return models.GetStorageDriver(storage.StorageType).OnDiskReset(ctx, userCred, disk, snapshot, data)
}

func (self *SKVMRegionDriver) RequestUpdateSnapshotPolicy(ctx context.Context,
	userCred mcclient.TokenCredential, sp *models.SSnapshotPolicy, input cloudprovider.SnapshotPolicyInput,
	task taskman.ITask) error {

	return nil
}

func (self *SKVMRegionDriver) ValidateCreateSnapshopolicyDiskData(ctx context.Context,
	userCred mcclient.TokenCredential, disk *models.SDisk, snapshotPolicy *models.SSnapshotPolicy) error {

	err := self.SBaseRegionDriver.ValidateCreateSnapshopolicyDiskData(ctx, userCred, disk, snapshotPolicy)
	if err != nil {
		return err
	}

	if snapshotPolicy.RetentionDays < -1 || snapshotPolicy.RetentionDays == 0 || snapshotPolicy.RetentionDays > options.Options.RetentionDaysLimit {
		return httperrors.NewInputParameterError("Retention days must in 1~%d or -1", options.Options.RetentionDaysLimit)
	}

	repeatWeekdays := models.SnapshotPolicyManager.RepeatWeekdaysToIntArray(snapshotPolicy.RepeatWeekdays)
	timePoints := models.SnapshotPolicyManager.TimePointsToIntArray(snapshotPolicy.TimePoints)

	if len(repeatWeekdays) > options.Options.RepeatWeekdaysLimit {
		return httperrors.NewInputParameterError("repeat_weekdays only contains %d days at most",
			options.Options.RepeatWeekdaysLimit)
	}

	if len(timePoints) > options.Options.TimePointsLimit {
		return httperrors.NewInputParameterError("time_points only contains %d points at most", options.Options.TimePointsLimit)
	}
	return nil
}

func (self *SKVMRegionDriver) RequestApplySnapshotPolicy(ctx context.Context, userCred mcclient.TokenCredential, task taskman.ITask, disk *models.SDisk, sp *models.SSnapshotPolicy, data jsonutils.JSONObject) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		data := jsonutils.NewDict()
		data.Add(jsonutils.NewString(sp.GetId()), "snapshotpolicy_id")
		return data, nil
	})
	return nil
}

func (self *SKVMRegionDriver) RequestCancelSnapshotPolicy(ctx context.Context, userCred mcclient.TokenCredential, task taskman.ITask, disk *models.SDisk, sp *models.SSnapshotPolicy, data jsonutils.JSONObject) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		data := jsonutils.NewDict()
		data.Add(jsonutils.NewString(sp.GetId()), "snapshotpolicy_id")
		return data, nil
	})
	return nil
}

func (self *SKVMRegionDriver) OnSnapshotDelete(ctx context.Context, snapshot *models.SSnapshot, task taskman.ITask, data jsonutils.JSONObject) error {
	task.SetStage("OnKvmSnapshotDelete", nil)
	task.ScheduleRun(data)
	return nil
}

func (self *SKVMRegionDriver) RequestSyncDiskStatus(ctx context.Context, userCred mcclient.TokenCredential, disk *models.SDisk, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		storage := disk.GetStorage()
		host := storage.GetMasterHost()
		header := task.GetTaskRequestHeader()
		url := fmt.Sprintf("%s/disks/%s/%s/status", host.ManagerUri, storage.Id, disk.Id)
		_, res, err := httputils.JSONRequest(httputils.GetDefaultClient(), ctx, "GET", url, header, nil, false)
		if err != nil {
			return nil, err
		}
		var diskStatus string
		originStatus, _ := task.GetParams().GetString("origin_status")
		status, _ := res.GetString("status")
		if status == api.DISK_EXIST {
			diskStatus = originStatus
		} else {
			diskStatus = api.DISK_UNKNOWN
		}
		return nil, disk.SetStatus(userCred, diskStatus, "sync status")
	})
	return nil
}

func (self *SKVMRegionDriver) RequestSyncSnapshotStatus(ctx context.Context, userCred mcclient.TokenCredential, snapshot *models.SSnapshot, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		storage := snapshot.GetStorage()
		host := storage.GetMasterHost()
		header := task.GetTaskRequestHeader()
		url := fmt.Sprintf("%s/snapshots/%s/%s/%s/status", host.ManagerUri, storage.Id, snapshot.DiskId, snapshot.Id)
		_, res, err := httputils.JSONRequest(httputils.GetDefaultClient(), ctx, "GET", url, header, nil, false)
		if err != nil {
			return nil, err
		}
		var snapshotStatus string
		originStatus, _ := task.GetParams().GetString("origin_status")
		status, _ := res.GetString("status")
		if status == api.SNAPSHOT_EXIST {
			snapshotStatus = originStatus
		} else {
			snapshotStatus = api.SNAPSHOT_UNKNOWN
		}
		return nil, snapshot.SetStatus(userCred, snapshotStatus, "sync status")
	})
	return nil
}

func (self *SKVMRegionDriver) DealNatGatewaySpec(spec string) string {
	return spec
}
func (self *SKVMRegionDriver) RequestBindIPToNatgateway(ctx context.Context, task taskman.ITask, natgateway *models.SNatGateway,
	eipId string) error {

	return nil
}

func (self *SKVMRegionDriver) RequestUnBindIPFromNatgateway(ctx context.Context, task taskman.ITask,
	nat models.INatHelper, natgateway *models.SNatGateway) error {

	return nil
}

func (self *SKVMRegionDriver) RequestPreSnapshotPolicyApply(ctx context.Context, userCred mcclient.
	TokenCredential, task taskman.ITask, disk *models.SDisk, sp *models.SSnapshotPolicy, data jsonutils.JSONObject) error {

	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {

		return data, nil
	})
	return nil
}

func (self *SKVMRegionDriver) BindIPToNatgatewayRollback(ctx context.Context, eipId string) error {
	return nil
}

func (self *SKVMRegionDriver) ValidateCacheSecgroup(ctx context.Context, userCred mcclient.TokenCredential, secgroup *models.SSecurityGroup, vpc *models.SVpc, classic bool) error {
	return fmt.Errorf("No need to cache secgroup for onecloud region")
}

func (self *SKVMRegionDriver) RequestCreateElasticcache(ctx context.Context, userCred mcclient.TokenCredential, elasticcache *models.SElasticcache, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) ValidateCreateElasticcacheData(ctx context.Context, userCred mcclient.TokenCredential, ownerId mcclient.IIdentityProvider, data *jsonutils.JSONDict) (*jsonutils.JSONDict, error) {
	return data, nil
}

func (self *SKVMRegionDriver) RequestRestartElasticcache(ctx context.Context, userCred mcclient.TokenCredential, elasticcache *models.SElasticcache, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) RequestSyncElasticcache(ctx context.Context, userCred mcclient.TokenCredential, elasticcache *models.SElasticcache, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) RequestDeleteElasticcache(ctx context.Context, userCred mcclient.TokenCredential, elasticcache *models.SElasticcache, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) RequestChangeElasticcacheSpec(ctx context.Context, userCred mcclient.TokenCredential, ec *models.SElasticcache, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) RequestSetElasticcacheMaintainTime(ctx context.Context, userCred mcclient.TokenCredential, ec *models.SElasticcache, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) RequestElasticcacheChangeSpec(ctx context.Context, userCred mcclient.TokenCredential, elasticcache *models.SElasticcache, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) RequestUpdateElasticcacheAuthMode(ctx context.Context, userCred mcclient.TokenCredential, elasticcache *models.SElasticcache, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) RequestElasticcacheSetMaintainTime(ctx context.Context, userCred mcclient.TokenCredential, elasticcache *models.SElasticcache, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) RequestElasticcacheAllocatePublicConnection(ctx context.Context, userCred mcclient.TokenCredential, ec *models.SElasticcache, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) RequestElasticcacheReleasePublicConnection(ctx context.Context, userCred mcclient.TokenCredential, ec *models.SElasticcache, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) RequestElasticcacheFlushInstance(ctx context.Context, userCred mcclient.TokenCredential, ec *models.SElasticcache, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) RequestElasticcacheUpdateInstanceParameters(ctx context.Context, userCred mcclient.TokenCredential, ec *models.SElasticcache, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) RequestElasticcacheUpdateBackupPolicy(ctx context.Context, userCred mcclient.TokenCredential, ec *models.SElasticcache, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) ValidateCreateElasticcacheAccountData(ctx context.Context, userCred mcclient.TokenCredential, ownerId mcclient.IIdentityProvider, data *jsonutils.JSONDict) (*jsonutils.JSONDict, error) {
	return nil, nil
}

func (self *SKVMRegionDriver) ValidateCreateElasticcacheAclData(ctx context.Context, userCred mcclient.TokenCredential, ownerId mcclient.IIdentityProvider, data *jsonutils.JSONDict) (*jsonutils.JSONDict, error) {
	return nil, nil
}

func (self *SKVMRegionDriver) AllowCreateElasticcacheBackup(ctx context.Context, userCred mcclient.TokenCredential, ownerId mcclient.IIdentityProvider, elasticcache *models.SElasticcache) error {
	return fmt.Errorf("not support create kvm elastic cache backup")
}

func (self *SKVMRegionDriver) ValidateCreateElasticcacheBackupData(ctx context.Context, userCred mcclient.TokenCredential, ownerId mcclient.IIdentityProvider, data *jsonutils.JSONDict) (*jsonutils.JSONDict, error) {
	return nil, nil
}

func (self *SKVMRegionDriver) RequestCreateElasticcacheAccount(ctx context.Context, userCred mcclient.TokenCredential, elasticcacheAccount *models.SElasticcacheAccount, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) RequestCreateElasticcacheAcl(ctx context.Context, userCred mcclient.TokenCredential, elasticcacheAcl *models.SElasticcacheAcl, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) RequestCreateElasticcacheBackup(ctx context.Context, userCred mcclient.TokenCredential, elasticcacheBackup *models.SElasticcacheBackup, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) RequestDeleteElasticcacheAccount(ctx context.Context, userCred mcclient.TokenCredential, ea *models.SElasticcacheAccount, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) RequestDeleteElasticcacheAcl(ctx context.Context, userCred mcclient.TokenCredential, ea *models.SElasticcacheAcl, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) RequestDeleteElasticcacheBackup(ctx context.Context, userCred mcclient.TokenCredential, eb *models.SElasticcacheBackup, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) RequestElasticcacheAccountResetPassword(ctx context.Context, userCred mcclient.TokenCredential, ea *models.SElasticcacheAccount, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) RequestElasticcacheAclUpdate(ctx context.Context, userCred mcclient.TokenCredential, ea *models.SElasticcacheAcl, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) RequestElasticcacheBackupRestoreInstance(ctx context.Context, userCred mcclient.TokenCredential, ea *models.SElasticcacheBackup, task taskman.ITask) error {
	return nil
}

func (self *SKVMRegionDriver) AllowUpdateElasticcacheAuthMode(ctx context.Context, userCred mcclient.TokenCredential, ownerId mcclient.IIdentityProvider, elasticcache *models.SElasticcache) error {
	return fmt.Errorf("not support update kvm elastic cache auth_mode")
}

func (self *SKVMRegionDriver) RequestSyncBucketStatus(ctx context.Context, userCred mcclient.TokenCredential, bucket *models.SBucket, task taskman.ITask) error {
	taskman.LocalTaskRun(task, func() (jsonutils.JSONObject, error) {
		iBucket, err := bucket.GetIBucket()
		if err != nil {
			return nil, errors.Wrap(err, "bucket.GetIBucket")
		}

		return nil, bucket.SetStatus(userCred, iBucket.GetStatus(), "syncstatus")
	})
	return nil
}
