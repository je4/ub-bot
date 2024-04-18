package catalogue

import (
	"github.com/je4/ubcat/v2/pkg/schema"
)

type channelConfig struct {
	maxResults int64
}
type channelStatus struct {
	config         channelConfig
	result         []*schema.UBSchema
	lastQuery      string
	lastSearchType SearchType
	lastVector     []float32
	searchFunc     string
}

type cStatus map[string]*channelStatus

func (c cStatus) Get(channelID string) *channelStatus {
	if stat, ok := c[channelID]; !ok {
		stat = &channelStatus{
			config: channelConfig{
				maxResults: defaultResultSize,
			},
			result: []*schema.UBSchema{},
		}
		c[channelID] = stat
		return stat
	} else {
		return stat
	}
}
