/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"fmt"
	"net"

	"github.com/openshift/ingress-node-firewall/pkg/failsaferules"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

type empty struct{}
type uint32Set map[uint32]empty

// log is for logging in this package.
var (
	ingressnodefirewalllog = logf.Log.WithName("ingressnodefirewall-resource")
)

func (r *IngressNodeFirewall) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}

//+kubebuilder:webhook:path=/validate-ingress-nodefw-ingress-nodefw-v1alpha1-ingressnodefirewall,mutating=false,failurePolicy=fail,sideEffects=None,groups=ingress-nodefw.ingress-nodefw,resources=ingressnodefirewalls,verbs=create;update,versions=v1alpha1,name=vingressnodefirewall.kb.io,admissionReviewVersions=v1

var _ webhook.Validator = &IngressNodeFirewall{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *IngressNodeFirewall) ValidateCreate() error {
	ingressnodefirewalllog.Info("validate create", "name", r.Name)

	return validateIngressNodeFirewall(r)
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *IngressNodeFirewall) ValidateUpdate(old runtime.Object) error {
	ingressnodefirewalllog.Info("validate update", "name", r.Name)

	return validateIngressNodeFirewall(r)
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *IngressNodeFirewall) ValidateDelete() error {
	ingressnodefirewalllog.Info("validate delete", "name", r.Name)

	return nil
}

func validateIngressNodeFirewall(inf *IngressNodeFirewall) error {
	if allErrs := validateINFRules(inf.Spec.Ingress, inf.Name); len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: IngressNodeFirewall{}.Kind},
			inf.Name, allErrs)
	}
	return nil
}

func validateINFRules(infRules []IngressNodeFirewallRules, infName string) field.ErrorList {
	var allErrs field.ErrorList
	for infRulesIndex, infRule := range infRules {
		if newErrs := validatesourceCIDRs(allErrs, infRule.SourceCIDRs, infRulesIndex, infName); len(newErrs) > 0 {
			allErrs = append(allErrs, newErrs...)
		}

		if newErrs := validateRules(allErrs, infRule.FirewallProtocolRules, infRulesIndex, infName); len(newErrs) > 0 {
			allErrs = append(allErrs, newErrs...)
		}
	}
	return allErrs
}

func validatesourceCIDRs(allErrs field.ErrorList, sourceCIDRs []string, infRulesIndex int, infName string) field.ErrorList {
	if len(sourceCIDRs) == 0 {
		allErrs = append(allErrs,
			field.Invalid(field.NewPath("spec").Child("ingress").Index(infRulesIndex).Key("sourceCIDRs"),
				infName, fmt.Sprintf("must be at least one sourceCIDRs")))
	} else {
		for sourceCIDRSIndex, sourceCIDR := range sourceCIDRs {
			if isValid, reason := validateSourceCIDR(sourceCIDR); !isValid {
				allErrs = append(allErrs, field.Invalid(
					field.NewPath("spec").Child("ingress").Index(infRulesIndex).Key("sourceCIDRs").Index(sourceCIDRSIndex),
					infName, fmt.Sprintf("must be a valid IPV4 or IPV6 CIDR: %s", reason)))
			}
		}
	}
	return allErrs
}

func validateRules(allErrs field.ErrorList, rules []IngressNodeFirewallProtocolRule, infRulesIndex int,
	infName string) field.ErrorList {
	if err := validateRuleLength(rules, infRulesIndex, infName); err != nil {
		allErrs = append(allErrs, err)
	}
	if !orderIsUnique(rules) {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec").Child("ingress").Index(infRulesIndex).Key("rules"),
			infName, "must have unique order"))
	}
	for ruleIndex, rule := range rules {
		if err := validateRule(rule, infRulesIndex, ruleIndex, infName); err != nil {
			allErrs = append(allErrs, err)
		}
	}
	return allErrs
}

func validateRule(rule IngressNodeFirewallProtocolRule, infRulesIndex, ruleIndex int, infName string) *field.Error {
	if rule.Protocol == ProtocolTypeICMP {
		if isValid, reason := isValidICMPICMPV6Rule(rule); !isValid {
			return field.Invalid(field.NewPath("spec").Child("ingress").Index(infRulesIndex).Key("rules").Index(ruleIndex),
				infName, fmt.Sprintf("must be a valid ICMP(V6) rule: %s", reason))
		}
	}

	if rule.Protocol == ProtocolTypeTCP || rule.Protocol == ProtocolTypeUDP || rule.Protocol == ProtocolTypeSCTP {
		if isValid, reason := isValidTCPUDPSCTPRule(rule); !isValid {
			return field.Invalid(field.NewPath("spec").Child("ingress").Index(infRulesIndex).Key("rules").Index(ruleIndex),
				infName, fmt.Sprintf("must be a valid %s rule: %s", rule.Protocol, reason))
		}

		if isConflict, err := isConflictWithSafeRulesTransport(rule); !isConflict && err != nil {
			return field.Invalid(field.NewPath("spec").Child("ingress").Index(infRulesIndex).Key("rules").Index(ruleIndex),
				infName, fmt.Sprintf("must be a valid %s rule: %v", rule.Protocol, err))
		} else if isConflict && err != nil {
			return field.Forbidden(field.NewPath("spec").Child("ingress").Index(infRulesIndex).Key("rules").Index(ruleIndex),
				err.Error())
		}
	}
	return nil
}

func isConflictWithSafeRulesTransport(rule IngressNodeFirewallProtocolRule) (bool, error) {
	var failSafeRules []failsaferules.TransportProtoFailSafeRule
	var err error
	var start, end uint16

	if rule.Protocol == ProtocolTypeTCP {
		failSafeRules = failsaferules.GetTCP()
	} else if rule.Protocol == ProtocolTypeUDP {
		failSafeRules = failsaferules.GetUDP()
	} else {
		return false, fmt.Errorf("unable to determine conflict rules for unknown protocol: %q", rule.Protocol)
	}

	for _, failSafeRule := range failSafeRules {
		if rule.ProtocolRule == nil {
			return false, fmt.Errorf("expected ports to be defined for transport protocol")
		}
		if rule.ProtocolRule.IsRange() {
			start, end, err = rule.ProtocolRule.GetRange()
			if err != nil {
				return false, fmt.Errorf("failed to get rule ports range: %v", err)
			}
			if withinRange(failSafeRule.GetPort(), start, end) {
				return true, fmt.Errorf("port range is in conflict with access to %s", failSafeRule.GetServiceName())
			}
		} else {
			start, err = rule.ProtocolRule.GetPort()
			if err != nil {
				return false, err
			}
			if failSafeRule.GetPort() == start {
				return true, fmt.Errorf("port is in conflict with access to %s", failSafeRule.GetServiceName())
			}
		}
	}
	return false, nil
}

func validateRuleLength(infRules []IngressNodeFirewallProtocolRule, infRulesIndex int, infName string) *field.Error {
	if len(infRules) > failsaferules.MAX_INGRESS_RULES {
		return field.Invalid(field.NewPath("spec").Child("ingress").Index(infRulesIndex).Key("rules"),
			infName, fmt.Sprintf("must be no more than %d rules", failsaferules.MAX_INGRESS_RULES))
	}
	return nil
}

func validateSourceCIDR(sourceCIDR string) (bool, string) {
	if _, _, err := net.ParseCIDR(sourceCIDR); err != nil {
		return false, fmt.Sprintf("must define valid IPV4 or IPV6 CIDR: %s", err.Error())
	}
	return true, ""
}

func isValidICMPICMPV6Rule(rule IngressNodeFirewallProtocolRule) (bool, string) {
	if rule.ICMPRule == nil {
		return false, "no ICMP rules defined. Define icmpType/icmpCode"
	}

	if rule.ProtocolRule != nil {
		return false, "ports are erroneously defined"
	}
	return true, ""
}

func isValidTCPUDPSCTPRule(rule IngressNodeFirewallProtocolRule) (bool, string) {
	if rule.ProtocolRule == nil {
		return false, "no port defined"
	}

	if rule.ProtocolRule.IsRange() {
		// GetRange() validates that range is valid and emits an error if this is not the case
		_, _, err := rule.ProtocolRule.GetRange()
		if err != nil {
			return false, fmt.Sprintf("must be a valid port range: %s", err.Error())
		}
	} else {
		_, err := rule.ProtocolRule.GetPort()
		if err != nil {
			return false, fmt.Sprintf("must be a valid port: %s", err.Error())
		}
	}

	if rule.ICMPRule != nil {
		return false, "ICMP type/code defined for a non-ICMP(V6) rule"
	}
	return true, ""
}

func orderIsUnique(infRules []IngressNodeFirewallProtocolRule) bool {
	orderSet := uint32Set{}
	for _, rule := range infRules {
		orderSet[rule.Order] = empty{}
	}
	if len(orderSet) != len(infRules) {
		return false
	}
	return true
}

func withinRange(i, lowerBound, upperBound uint16) bool {
	return i >= lowerBound && i <= upperBound
}
