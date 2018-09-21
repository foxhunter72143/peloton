package util

import (
	"strings"

	mesos "code.uber.internal/infra/peloton/.gen/mesos/v1"
	"code.uber.internal/infra/peloton/.gen/peloton/private/hostmgr/hostsvc"
	log "github.com/sirupsen/logrus"
)

// LabelKeyToEnvVarName converts a task label key to an env var name
// Example: label key 'peloton.job_id' converted to env var name 'PELOTON_JOB_ID'
func LabelKeyToEnvVarName(labelKey string) string {
	return strings.ToUpper(strings.Replace(labelKey, ".", "_", 1))
}

// MesosOffersToHostOffers takes the Mesos Offer and returns the Host Offer
func MesosOffersToHostOffers(hostoffers map[string][]*mesos.Offer) []*hostsvc.HostOffer {
	hostOffers := make([]*hostsvc.HostOffer, 0, len(hostoffers))
	for hostname, offers := range hostoffers {
		if len(offers) <= 0 {
			log.WithField("host", hostname).
				Warn("Empty offer slice from host")
			continue
		}

		var resources []*mesos.Resource
		var attributes []*mesos.Attribute
		for _, offer := range offers {
			resources = append(resources, offer.GetResources()...)
			attributes = append(attributes, offer.GetAttributes()...)
		}

		hostOffer := hostsvc.HostOffer{
			Hostname:   hostname,
			AgentId:    offers[0].GetAgentId(),
			Attributes: attributes,
			Resources:  resources,
		}

		hostOffers = append(hostOffers, &hostOffer)
	}
	return hostOffers
}

// IsSlackResourceType validates is given resource type is supported slack resource.
func IsSlackResourceType(resourceType string, slackResourceTypes []string) bool {
	for _, rType := range slackResourceTypes {
		if strings.ToLower(rType) == strings.ToLower(resourceType) {
			return true
		}
	}
	return false
}
