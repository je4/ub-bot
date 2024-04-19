package catalogue

import (
	"bufio"
	"bytes"
	"context"
	"emperror.dev/errors"
	"fmt"
	"github.com/bwmarrin/discordgo"
	"github.com/dgraph-io/badger/v4"
	"github.com/elastic/go-elasticsearch/v8"
	"github.com/je4/ub-bot/v2/data"
	"github.com/je4/ub-bot/v2/pkg/discord"
	"github.com/je4/ubcat/v2/pkg/index"
	"github.com/je4/ubcat/v2/pkg/schema"
	"github.com/je4/utils/v2/pkg/openai"
	"github.com/je4/utils/v2/pkg/zLogger"
	oai "github.com/sashabaranov/go-openai"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"text/template"
)

const (
	SearchTypeSimple SearchType = iota
	SearchTypeEmbeddingMARC
	SearchTypeEmbeddingProse
	SearchTypeEmbeddingJSON
)
const (
	defaultResultSize = 9
	maxResultSize     = 100
)

type SearchType int

func NewCatalogue(elastic *elasticsearch.TypedClient, elasticIndex string, badgerDB *badger.DB, openaiApiKey string, prefix string, logger zLogger.ZLogger) *Catalog {
	kvBadger := openai.NewKVBadger(badgerDB)
	client := openai.NewClientV2(openaiApiKey, kvBadger, logger)
	cat := &Catalog{
		ubClient:     index.NewClient(elasticIndex, elastic),
		client:       client,
		logger:       logger,
		status:       cStatus{},
		prefix:       prefix,
		tmpl:         template.Must(template.New("embedding.gotmpl").Parse(data.TextTemplate)),
		channelMutex: map[string]*sync.Mutex{},
	}
	return cat
}

type Catalog struct {
	ubClient     *index.Client
	client       *openai.ClientV2
	logger       zLogger.ZLogger
	status       cStatus
	prefix       string
	tmpl         *template.Template
	channelMutex map[string]*sync.Mutex
}

var channelFilter = regexp.MustCompile(`^filter-([^-]+)-(.+)$`)

func FilterFromChannelName(name string) map[string]string {
	filter := map[string]string{}
	filterMatch := channelFilter.FindStringSubmatch(name)
	if filterMatch != nil && filterMatch[1] != "" && filterMatch[2] != "" {
		filter[strings.ReplaceAll(filterMatch[1], "_", ".")] = filterMatch[2] + "*"
	}
	return filter
}

var channelTopicFilter = regexp.MustCompile(`^([^:]+):(.+)$`)

func FilterFromChannelTopic(topic string) map[string]string {
	filter := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(topic))
	for scanner.Scan() {
		line := scanner.Text()
		if matches := channelTopicFilter.FindStringSubmatch(line); matches != nil {
			filter[strings.TrimSpace(matches[1])] = strings.TrimSpace(matches[2])
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Printf("error occurred: %v\n", err)
	}
	return filter
}

func (cat *Catalog) tryLock(channelID string) bool {
	if _, ok := cat.channelMutex[channelID]; !ok {
		cat.channelMutex[channelID] = &sync.Mutex{}
	}
	return cat.channelMutex[channelID].TryLock()
}

func (cat *Catalog) lock(channelID string) {
	if _, ok := cat.channelMutex[channelID]; !ok {
		cat.channelMutex[channelID] = &sync.Mutex{}
	}
	cat.channelMutex[channelID].Lock()
}

func (cat *Catalog) unlock(channelID string) {
	if _, ok := cat.channelMutex[channelID]; !ok {
		return
	}
	cat.channelMutex[channelID].Unlock()
}

func (cat *Catalog) GetEmbedding(queryString string) (embedding []float32, resultErr error) {
	e, err := cat.client.CreateEmbedding(queryString, oai.SmallEmbedding3)
	if err != nil {
		resultErr = err
		return
	}
	embedding = e.Embedding
	return
}

func (cat *Catalog) Query2Embedding(queryString string) (string, error) {
	result, err := cat.client.Query2EmbeddingQuery(queryString)
	if err != nil {
		return "", errors.Wrap(err, "cannot create embedding query")
	}
	return result, nil
}

func (cat *Catalog) GetDocuments(identifier ...string) (map[string]*schema.UBSchema, error) {
	return cat.ubClient.GetDocuments(context.Background(), identifier...)
}

func (cat *Catalog) Search(queryString string, filter map[string]string, embedding []float32, searchType SearchType, from, num int64) (*index.Result, error) {
	var vectorMarc, vectorProse, vectorJSON []float32
	if searchType != SearchTypeSimple {
		if embedding == nil {
			return nil, errors.Errorf("embedding is nil")
		}
		switch searchType {
		case SearchTypeEmbeddingMARC:
			vectorMarc = embedding
		case SearchTypeEmbeddingProse:
			vectorProse = embedding
		case SearchTypeEmbeddingJSON:
			vectorJSON = embedding
		default:
			return nil, errors.Errorf("unknown search type %v", searchType)
		}
	}
	res, err := cat.ubClient.Search(context.Background(), queryString, filter, vectorMarc, vectorJSON, vectorProse, from, num)
	if err != nil {
		return nil, errors.Wrap(err, "cannot search")
	}
	return res, nil
}
func (cat *Catalog) SearchKNN(filter map[string]string, embedding []float32, searchType SearchType, k int64, numCandidates int64) (*index.Result, error) {
	var field string
	if embedding == nil {
		return nil, errors.Errorf("embedding is nil")
	}
	switch searchType {
	case SearchTypeEmbeddingMARC:
		field = "embedding_marc"
	case SearchTypeEmbeddingProse:
		field = "embedding_prose"
	case SearchTypeEmbeddingJSON:
		field = "embedding_json"
	default:
		return nil, errors.Errorf("unknown search type %v", searchType)
	}
	res, err := cat.ubClient.SearchKNN(context.Background(), filter, embedding, field, k, numCandidates)
	if err != nil {
		return nil, errors.Wrap(err, "cannot search")
	}
	return res, nil
}

// todo: create regexp which fits all cases
var idRegexp = regexp.MustCompile(`^(99.*5504)$`)

func (cat *Catalog) Result2MessageEmbed(result *index.Result, stat *channelStatus) ([]*discordgo.MessageEmbed, error) {
	var embeds = []*discordgo.MessageEmbed{}

	embed := &discordgo.MessageEmbed{
		Author: &discordgo.MessageEmbedAuthor{
			Name: "ub-bot",
		},
		Title: "Query Results",
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:  "Query",
				Value: stat.lastQuery,
			},
			{
				Name:  "Total Hits",
				Value: fmt.Sprintf("%d", result.Total),
			},
		},
	}
	if !strings.HasPrefix(stat.lastQuery, "similar:") {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:  "Swisscovery Search",
			Value: fmt.Sprintf("https://basel.swisscovery.org/discovery/search?query=any,contains,%s&tab=UBS&search_scope=UBS&vid=41SLSP_UBS:live&offset=0", url.QueryEscape(stat.lastQuery)),
		})
	}
	embeds = append(embeds, embed)
	start := len(stat.result)
	var key int
	for _, entry := range result.Docs {
		stat.result = append(stat.result, entry)
		embed := &discordgo.MessageEmbed{
			Author: &discordgo.MessageEmbedAuthor{
				Name: fmt.Sprintf("%d - %f - %s", start+key, entry.Score_, entry.Id_),
			},
			Title:  entry.GetMainTitle(),
			Fields: []*discordgo.MessageEmbedField{},
		}
		if len(embed.Title) > 256 {
			embed.Title = embed.Title[:253] + "..."
		}
		var urlStr string
		if entry.UBSchema001.Mapping != nil && entry.UBSchema001.Mapping.RecordIdentifier != nil {
			for _, id := range entry.UBSchema001.Mapping.RecordIdentifier {
				if strings.HasPrefix(id, "(EXLNZ-41SLSP_NETWORK)") {
					urlStr = fmt.Sprintf("https://basel.swisscovery.org/discovery/fulldisplay?docid=alma%s&context=L&vid=41SLSP_UBS:live", id[22:])
					break
				}
			}
			if urlStr == "" {
				for _, id := range entry.UBSchema001.Mapping.RecordIdentifier {
					if matches := idRegexp.FindStringSubmatch(id); matches != nil {
						urlStr = fmt.Sprintf("https://basel.swisscovery.org/discovery/fulldisplay?docid=alma%s&context=L&vid=41SLSP_UBS:live", matches[1])
						break
					}
				}
			}
		}
		if urlStr != "" {
			embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
				Value: urlStr,
			})
		}
		for role, persons := range entry.GetPersons() {
			ps := []string{}
			for _, p := range persons {
				if p.Date != "" {
					ps = append(ps, fmt.Sprintf("%s (%s)", p.Name, p.Date))
				} else {
					ps = append(ps, p.Name)
				}
			}
			embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
				Name:  role,
				Value: strings.Join(ps, "; "),
			})
		}
		embeds = append(embeds, embed)
		key++
	}
	return embeds, nil
}

func (cat *Catalog) CommandSearch() (cmdFunc discord.CommandCreate, appCmd *discordgo.ApplicationCommand) {
	appCmd = &discordgo.ApplicationCommand{
		Name:        cat.prefix + "search",
		Description: "Search the catalogue",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type: discordgo.ApplicationCommandOptionString,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{
						Name:  "Marc Vector",
						Value: "marc",
					},
					{
						Name:  "Prose Vector",
						Value: "prose",
					},
					{
						Name:  "JSON Vector",
						Value: "json",
					},
					{
						Name:  "Simple Elastic Query",
						Value: "simple",
					},
				},
				Name:        "querytype",
				Description: "Query Type",
				Required:    true,
			},
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "query",
				Description: "Query to ask for",
				Required:    true,
			},
			{
				Type:        discordgo.ApplicationCommandOptionBoolean,
				Name:        "magic",
				Description: "Ask AI for better query before searching",
				Required:    false,
			},
		},
	}
	cmdFunc = func(i *discord.Interaction) {
		// get the search query from the user
		data := i.ApplicationCommandData()
		cat.logger.Debug().Msgf("command name: %s", data.Name)

		if cat.tryLock(i.ChannelID) == false {
			if err := i.SendInteractionResponseMessage("Please wait for the previous search to finish"); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}
		go func() {
			defer cat.unlock(i.ChannelID)

			var sType, query string
			var magic bool
			for _, opt := range data.Options {
				switch opt.Name {
				case "querytype":
					sType = opt.StringValue()
				case "query":
					query = opt.StringValue()
				case "magic":
					magic = opt.BoolValue()
				}
			}
			if sType == "" || query == "" {
				if err := i.SendInteractionResponseMessage("Please provide search type and query"); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}

			session := i.GetSession()
			channel, err := session.State.Channel(i.ChannelID)
			if err != nil {
				cat.logger.Error().Msgf("Error getting channel: %v", err)
				if err := i.SendChannelMessage(fmt.Sprintf("Error getting channel: %v", err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			filter := FilterFromChannelTopic(channel.Topic)

			msg := fmt.Sprintf("Searching for %s: %s", sType, query)
			msg += "\nFilter:\n"
			for k, v := range filter {
				msg += fmt.Sprintf("%s: %s\n", k, v)
			}
			if err := i.SendInteractionResponseMessage(msg); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}

			var newQuery = query
			if magic {
				var err error
				cat.logger.Debug().Msgf("magic query: %s", query)
				newQuery, err = cat.Query2Embedding(query)
				if err != nil {
					cat.logger.Error().Msgf("Error converting query: %v", err)
					if err := i.SendChannelMessage(fmt.Sprintf("Error converting query: %v", err)); err != nil {
						cat.logger.Error().Msgf("Error sending response: %v", err)
					}
					return
				}
				cat.logger.Debug().Msgf("new query: %s", newQuery)
			}

			var searchType SearchType
			var embedding []float32
			switch sType {
			case "marc":
				embedding, err = cat.GetEmbedding(newQuery)
				searchType = SearchTypeEmbeddingMARC
			case "prose":
				embedding, err = cat.GetEmbedding(newQuery)
				searchType = SearchTypeEmbeddingProse
			case "json":
				embedding, err = cat.GetEmbedding(newQuery)
				searchType = SearchTypeEmbeddingJSON
			case "simple":
				searchType = SearchTypeSimple
			default:
				if err := i.SendChannelMessage(fmt.Sprintf("Unknown search type %s", sType)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			if err != nil {
				cat.logger.Error().Msgf("Error getting embedding: %v", err)
				if err := i.SendChannelMessage(fmt.Sprintf("Error getting embedding: %v", err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}

			stat := cat.status.Get(i.ChannelID)
			result, err := cat.Search(newQuery, filter, embedding, searchType, 0, stat.config.maxResults)
			if err != nil {
				cat.logger.Error().Msgf("Error searching: %v", err)
				if err := i.SendChannelMessage(fmt.Sprintf("Error searching: %v", err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			stat.result = []*schema.UBSchema{}
			stat.lastQuery = newQuery
			stat.lastSearchType = searchType
			stat.lastVector = embedding
			stat.searchFunc = appCmd.Name

			embeds, err := cat.Result2MessageEmbed(result, stat)
			if err != nil {
				cat.logger.Error().Msgf("Error creating response: %v", err)
				if err := i.SendChannelMessage(fmt.Sprintf("Error creating response: %v", err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			cat.logger.Info().Msgf("sending %d embeds", len(embeds))
			if err := i.SendChannelEmbeds(embeds); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
				if err := i.SendInteractionResponseMessage(fmt.Sprintf("Error sending response: %v", err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
		}()
	}
	return
}
func (cat *Catalog) CommandSearchKNN() (cmdFunc discord.CommandCreate, appCmd *discordgo.ApplicationCommand) {
	appCmd = &discordgo.ApplicationCommand{
		Name:        cat.prefix + "searchknn",
		Description: "Search the catalogue",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type: discordgo.ApplicationCommandOptionString,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{
						Name:  "Marc Vector",
						Value: "marc",
					},
					{
						Name:  "Prose Vector",
						Value: "prose",
					},
					{
						Name:  "JSON Vector",
						Value: "json",
					},
					{
						Name:  "Simple Elastic Query",
						Value: "simple",
					},
				},
				Name:        "querytype",
				Description: "Query Type",
				Required:    true,
			},
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "query",
				Description: "Query to ask for",
				Required:    true,
			},
			{
				Type:        discordgo.ApplicationCommandOptionBoolean,
				Name:        "magic",
				Description: "Ask AI for better query before searching",
				Required:    false,
			},
		},
	}
	cmdFunc = func(i *discord.Interaction) {
		// get the search query from the user
		data := i.ApplicationCommandData()
		cat.logger.Debug().Msgf("command name: %s", data.Name)

		if cat.tryLock(i.ChannelID) == false {
			if err := i.SendInteractionResponseMessage("Please wait for the previous search to finish"); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}
		go func() {
			defer cat.unlock(i.ChannelID)

			var sType, query string
			var magic bool
			for _, opt := range data.Options {
				switch opt.Name {
				case "querytype":
					sType = opt.StringValue()
				case "query":
					query = opt.StringValue()
				case "magic":
					magic = opt.BoolValue()
				}
			}
			if sType == "" || query == "" {
				if err := i.SendInteractionResponseMessage("Please provide search type and query"); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}

			session := i.GetSession()
			channel, err := session.State.Channel(i.ChannelID)
			if err != nil {
				cat.logger.Error().Msgf("Error getting channel: %v", err)
				if err := i.SendChannelMessage(fmt.Sprintf("Error getting channel: %v", err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			filter := FilterFromChannelTopic(channel.Topic)

			msg := fmt.Sprintf("Searching for %s: %s", sType, query)
			msg += "\nFilter:\n"
			for k, v := range filter {
				msg += fmt.Sprintf("%s: %s\n", k, v)
			}
			if err := i.SendInteractionResponseMessage(msg); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}

			var newQuery = query
			if magic {
				var err error
				cat.logger.Debug().Msgf("magic query: %s", query)
				newQuery, err = cat.Query2Embedding(query)
				if err != nil {
					cat.logger.Error().Msgf("Error converting query: %v", err)
					if err := i.SendChannelMessage(fmt.Sprintf("Error converting query: %v", err)); err != nil {
						cat.logger.Error().Msgf("Error sending response: %v", err)
					}
					return
				}
				cat.logger.Debug().Msgf("new query: %s", newQuery)
			}

			var searchType SearchType
			var embedding []float32
			switch sType {
			case "marc":
				embedding, err = cat.GetEmbedding(newQuery)
				searchType = SearchTypeEmbeddingMARC
			case "prose":
				embedding, err = cat.GetEmbedding(newQuery)
				searchType = SearchTypeEmbeddingProse
			case "json":
				embedding, err = cat.GetEmbedding(newQuery)
				searchType = SearchTypeEmbeddingJSON
			case "simple":
				searchType = SearchTypeSimple
			default:
				if err := i.SendChannelMessage(fmt.Sprintf("Unknown search type %s", sType)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			if err != nil {
				cat.logger.Error().Msgf("Error getting embedding: %v", err)
				if err := i.SendChannelMessage(fmt.Sprintf("Error getting embedding: %v", err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}

			stat := cat.status.Get(i.ChannelID)
			result, err := cat.SearchKNN(filter, embedding, searchType, stat.config.maxResults, stat.config.maxResults)
			if err != nil {
				cat.logger.Error().Msgf("Error searching: %v", err)
				if err := i.SendChannelMessage(fmt.Sprintf("Error searching: %v", err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			stat.result = []*schema.UBSchema{}
			stat.lastQuery = newQuery
			stat.lastSearchType = searchType
			stat.lastVector = embedding
			stat.searchFunc = appCmd.Name

			embeds, err := cat.Result2MessageEmbed(result, stat)
			if err != nil {
				cat.logger.Error().Msgf("Error creating response: %v", err)
				if err := i.SendChannelMessage(fmt.Sprintf("Error creating response: %v", err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			cat.logger.Info().Msgf("sending %d embeds", len(embeds))
			if err := i.SendChannelEmbeds(embeds); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
				if err := i.SendInteractionResponseMessage(fmt.Sprintf("Error sending response: %v", err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
		}()
	}
	return
}

func (cat *Catalog) CommandSimilar() (cmdFunc discord.CommandCreate, appCmd *discordgo.ApplicationCommand) {
	appCmd = &discordgo.ApplicationCommand{
		Name:        cat.prefix + "similar",
		Description: "search similar object based on marc embedding",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type: discordgo.ApplicationCommandOptionString,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{
						Name:  "Marc Vector",
						Value: "marc",
					},
					{
						Name:  "Prose Vector",
						Value: "prose",
					},
					{
						Name:  "JSON Vector",
						Value: "json",
					},
				},
				Name:        "querytype",
				Description: "Query Type",
				Required:    true,
			},
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "resultid",
				Description: "Result ID from previous search or full elastic id",
				Required:    true,
			},
		},
	}
	cmdFunc = func(i *discord.Interaction) {
		data := i.ApplicationCommandData()
		if len(data.Options) < 2 {
			if err := i.SendInteractionResponseMessage("Please provide search type and result ID"); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}
		sType := data.Options[0].StringValue()
		resultIDStr := data.Options[1].StringValue()
		resultID := -1
		var err error
		var searchType SearchType
		var vector []float32
		var lastResult *schema.UBSchema
		stat := cat.status.Get(i.ChannelID)
		if resultID, err = strconv.Atoi(resultIDStr); err == nil && resultID < 100 {
			if len(stat.result) == 0 {
				if err := i.SendInteractionResponseMessage("No search results available"); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			if resultID < 0 || int(resultID) >= len(stat.result) {
				if err := i.SendInteractionResponseMessage("Invalid result ID"); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			lastResult = stat.result[resultID]
			stat.lastQuery = fmt.Sprintf("similar:%d - %s", resultID, lastResult.GetMainTitle())
			cat.logger.Debug().Msgf("result ID: %d", resultID)
		} else {
			docs, err := cat.GetDocuments(resultIDStr)
			if err != nil {
				cat.logger.Error().Msgf("Error getting document %s: %v", resultIDStr, err)
				if err := i.SendInteractionResponseMessage(fmt.Sprintf("Error getting document %s: %v", resultIDStr, err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			if len(docs) != 1 {
				cat.logger.Error().Msgf("Invalid document ID %s", resultIDStr)
				if err := i.SendInteractionResponseMessage(fmt.Sprintf("Invalid document ID %s", resultIDStr)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			var ok bool
			lastResult, ok = docs[resultIDStr]
			if !ok {
				cat.logger.Error().Msgf("Document %s not found", resultIDStr)
				if err := i.SendInteractionResponseMessage(fmt.Sprintf("Document %s not found", resultIDStr)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			stat.lastQuery = fmt.Sprintf("similar:%s - %s", resultIDStr, lastResult.GetMainTitle())

			cat.logger.Debug().Msgf("result ID: %s", resultIDStr)
		}

		if lastResult == nil {
			cat.logger.Error().Msgf("No last result available")
			if err := i.SendInteractionResponseMessage("No last result available"); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}

		switch sType {
		case "marc":
			searchType = SearchTypeEmbeddingMARC
			vector = lastResult.EmbeddingMarc
		case "prose":
			searchType = SearchTypeEmbeddingProse
			vector = lastResult.EmbeddingProse
		case "json":
			searchType = SearchTypeEmbeddingJSON
			vector = lastResult.EmbeddingJson
		default:
			if err := i.SendInteractionResponseMessage(fmt.Sprintf("Unknown search type %s", sType)); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}
		stat.result = []*schema.UBSchema{}
		stat.lastSearchType = searchType
		stat.lastVector = vector

		session := i.GetSession()
		channel, err := session.State.Channel(i.ChannelID)
		if err != nil {
			cat.logger.Error().Msgf("Error getting channel: %v", err)
			if err := i.SendChannelMessage(fmt.Sprintf("Error getting channel: %v", err)); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}
		filter := FilterFromChannelTopic(channel.Topic)

		msg := fmt.Sprintf("searching %s similarities for: %s", sType, lastResult.GetMainTitle())
		msg += "\nFilter:\n"
		for k, v := range filter {
			msg += fmt.Sprintf("%s: %s\n", k, v)
		}
		if err := i.SendInteractionResponseMessage(msg); err != nil {
			cat.logger.Error().Msgf("Error sending response: %v", err)
		}
		cat.logger.Debug().Msgf("searching %s similarities for: %s", sType, lastResult.GetMainTitle())
		if cat.tryLock(i.ChannelID) == false {
			if err := i.SendInteractionResponseMessage("Please wait for the previous search to finish"); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}
		go func() {
			defer cat.unlock(i.ChannelID)

			result, err := cat.Search("", filter, vector, searchType, 0, stat.config.maxResults)
			if err != nil {
				cat.logger.Error().Msgf("Error searching: %v", err)
				if err := i.SendChannelMessage(fmt.Sprintf("Error searching: %v", err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}

			embeds, err := cat.Result2MessageEmbed(result, stat)
			if err != nil {
				cat.logger.Error().Msgf("Error creating response: %v", err)
				if err := i.SendChannelMessage(fmt.Sprintf("Error creating response: %v", err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			if err := i.SendChannelEmbeds(embeds); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
				if err := i.SendChannelMessage(fmt.Sprintf("Error sending response: %v", err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
		}()
	}
	return
}
func (cat *Catalog) CommandSimilarKNN() (cmdFunc discord.CommandCreate, appCmd *discordgo.ApplicationCommand) {
	appCmd = &discordgo.ApplicationCommand{
		Name:        cat.prefix + "similarknn",
		Description: "search similar object based on marc embedding",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type: discordgo.ApplicationCommandOptionString,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{
						Name:  "Marc Vector",
						Value: "marc",
					},
					{
						Name:  "Prose Vector",
						Value: "prose",
					},
					{
						Name:  "JSON Vector",
						Value: "json",
					},
				},
				Name:        "querytype",
				Description: "Query Type",
				Required:    true,
			},
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "resultid",
				Description: "Result ID from previous search or full elastic id",
				Required:    true,
			},
		},
	}
	cmdFunc = func(i *discord.Interaction) {
		data := i.ApplicationCommandData()
		if len(data.Options) < 2 {
			if err := i.SendInteractionResponseMessage("Please provide search type and result ID"); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}
		sType := data.Options[0].StringValue()
		resultIDStr := data.Options[1].StringValue()
		resultID := -1
		var err error
		var searchType SearchType
		var vector []float32
		var lastResult *schema.UBSchema
		stat := cat.status.Get(i.ChannelID)
		if resultID, err = strconv.Atoi(resultIDStr); err == nil && resultID < 100 {
			if len(stat.result) == 0 {
				if err := i.SendInteractionResponseMessage("No search results available"); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			if resultID < 0 || int(resultID) >= len(stat.result) {
				if err := i.SendInteractionResponseMessage("Invalid result ID"); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			lastResult = stat.result[resultID]
			stat.lastQuery = fmt.Sprintf("similar:%d - %s", resultID, lastResult.GetMainTitle())
			cat.logger.Debug().Msgf("result ID: %d", resultID)
		} else {
			docs, err := cat.GetDocuments(resultIDStr)
			if err != nil {
				cat.logger.Error().Msgf("Error getting document %s: %v", resultIDStr, err)
				if err := i.SendInteractionResponseMessage(fmt.Sprintf("Error getting document %s: %v", resultIDStr, err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			if len(docs) != 1 {
				cat.logger.Error().Msgf("Invalid document ID %s", resultIDStr)
				if err := i.SendInteractionResponseMessage(fmt.Sprintf("Invalid document ID %s", resultIDStr)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			var ok bool
			lastResult, ok = docs[resultIDStr]
			if !ok {
				cat.logger.Error().Msgf("Document %s not found", resultIDStr)
				if err := i.SendInteractionResponseMessage(fmt.Sprintf("Document %s not found", resultIDStr)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			stat.lastQuery = fmt.Sprintf("similar:%s - %s", resultIDStr, lastResult.GetMainTitle())

			cat.logger.Debug().Msgf("result ID: %s", resultIDStr)
		}

		if lastResult == nil {
			cat.logger.Error().Msgf("No last result available")
			if err := i.SendInteractionResponseMessage("No last result available"); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}

		switch sType {
		case "marc":
			searchType = SearchTypeEmbeddingMARC
			vector = lastResult.EmbeddingMarc
		case "prose":
			searchType = SearchTypeEmbeddingProse
			vector = lastResult.EmbeddingProse
		case "json":
			searchType = SearchTypeEmbeddingJSON
			vector = lastResult.EmbeddingJson
		default:
			if err := i.SendInteractionResponseMessage(fmt.Sprintf("Unknown search type %s", sType)); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}
		stat.result = []*schema.UBSchema{}
		stat.lastSearchType = searchType
		stat.lastVector = vector

		session := i.GetSession()
		channel, err := session.State.Channel(i.ChannelID)
		if err != nil {
			cat.logger.Error().Msgf("Error getting channel: %v", err)
			if err := i.SendChannelMessage(fmt.Sprintf("Error getting channel: %v", err)); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}
		filter := FilterFromChannelTopic(channel.Topic)

		msg := fmt.Sprintf("searching %s similarities for: %s", sType, lastResult.GetMainTitle())
		msg += "\nFilter:\n"
		for k, v := range filter {
			msg += fmt.Sprintf("%s: %s\n", k, v)
		}
		if err := i.SendInteractionResponseMessage(msg); err != nil {
			cat.logger.Error().Msgf("Error sending response: %v", err)
		}
		cat.logger.Debug().Msgf("searching %s similarities for: %s", sType, lastResult.GetMainTitle())
		if cat.tryLock(i.ChannelID) == false {
			if err := i.SendInteractionResponseMessage("Please wait for the previous search to finish"); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}
		go func() {
			defer cat.unlock(i.ChannelID)

			result, err := cat.SearchKNN(filter, vector, searchType, stat.config.maxResults, stat.config.maxResults)
			if err != nil {
				cat.logger.Error().Msgf("Error searching: %v", err)
				if err := i.SendChannelMessage(fmt.Sprintf("Error searching: %v", err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}

			embeds, err := cat.Result2MessageEmbed(result, stat)
			if err != nil {
				cat.logger.Error().Msgf("Error creating response: %v", err)
				if err := i.SendChannelMessage(fmt.Sprintf("Error creating response: %v", err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			if err := i.SendChannelEmbeds(embeds); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
				if err := i.SendChannelMessage(fmt.Sprintf("Error sending response: %v", err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
		}()
	}
	return
}

func (cat *Catalog) CommandMagic() (cmdFunc discord.CommandCreate, appCmd *discordgo.ApplicationCommand) {
	appCmd = &discordgo.ApplicationCommand{
		Name:        cat.prefix + "magic",
		Description: "Magic search",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "query",
				Description: "Query to do magic with",
				Required:    true,
			},
		},
	}
	m := sync.Mutex{}
	cmdFunc = func(i *discord.Interaction) {
		data := i.ApplicationCommandData()
		cat.logger.Debug().Msgf("command name: %s", data.Name)
		if len(data.Options) < 1 {
			if err := i.SendInteractionResponseMessage("Please provide query"); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}
		query := data.Options[0].StringValue()
		cat.logger.Debug().Msgf("magic query: %s", query)
		if m.TryLock() == false {
			if err := i.SendInteractionResponseMessage("Please wait for the previous search to finish"); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}
		go func() {
			defer m.Unlock()
			newQuery, err := cat.Query2Embedding(query)
			if err != nil {
				cat.logger.Error().Msgf("Error converting query: %v", err)
				if err := i.SendChannelMessage(fmt.Sprintf("Error converting query: %v", err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			cat.logger.Debug().Msgf("new query: %s", newQuery)
			if err := i.SendChannelMessage(newQuery); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
		}()
	}
	return
}

func (cat *Catalog) CommandText() (cmdFunc discord.CommandCreate, appCmd *discordgo.ApplicationCommand) {
	appCmd = &discordgo.ApplicationCommand{
		Name:        cat.prefix + "text",
		Description: "show base text of prose embedding",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "resultid",
				Description: "Result ID from previous search or full elastic id",
				Required:    true,
			},
		},
	}
	cmdFunc = func(i *discord.Interaction) {
		data := i.ApplicationCommandData()
		cat.logger.Debug().Msgf("command name: %s", data.Name)
		if len(data.Options) < 1 {
			if err := i.SendInteractionResponseMessage("Please provide query"); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}

		resultIDStr := data.Options[0].StringValue()
		resultID := -1
		var err error
		var lastResult *schema.UBSchema
		stat := cat.status.Get(i.ChannelID)
		if resultID, err = strconv.Atoi(resultIDStr); err == nil && resultID < 100 {
			if len(stat.result) == 0 {
				if err := i.SendInteractionResponseMessage("No search results available"); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			if resultID < 0 || int(resultID) >= len(stat.result) {
				if err := i.SendInteractionResponseMessage("Invalid result ID"); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			lastResult = stat.result[resultID]
			stat.lastQuery = fmt.Sprintf("similar:%d - %s", resultID, lastResult.GetMainTitle())
			cat.logger.Debug().Msgf("result ID: %d", resultID)
		} else {
			docs, err := cat.GetDocuments(resultIDStr)
			if err != nil {
				cat.logger.Error().Msgf("Error getting document %s: %v", resultIDStr, err)
				if err := i.SendInteractionResponseMessage(fmt.Sprintf("Error getting document %s: %v", resultIDStr, err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			if len(docs) != 1 {
				cat.logger.Error().Msgf("Invalid document ID %s", resultIDStr)
				if err := i.SendInteractionResponseMessage(fmt.Sprintf("Invalid document ID %s", resultIDStr)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			var ok bool
			lastResult, ok = docs[resultIDStr]
			if !ok {
				cat.logger.Error().Msgf("Document %s not found", resultIDStr)
				if err := i.SendInteractionResponseMessage(fmt.Sprintf("Document %s not found", resultIDStr)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}

			cat.logger.Debug().Msgf("result ID: %s", resultIDStr)
		}
		if lastResult == nil {
			cat.logger.Error().Msgf("No last result available")
			if err := i.SendInteractionResponseMessage("No last result available"); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}

		buf := bytes.NewBuffer(nil)
		if err := cat.tmpl.Execute(buf, lastResult); err != nil {
			cat.logger.Error().Msgf("Error executing template: %v", err)
			if err := i.SendInteractionResponseMessage(fmt.Sprintf("Error executing template: %v", err)); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}
		if err := i.SendInteractionResponseMessage(buf.String()); err != nil {
			cat.logger.Error().Msgf("Error sending response: %v", err)
		}
	}
	return
}

func (cat *Catalog) CommandMore() (cmdFunc discord.CommandCreate, appCmd *discordgo.ApplicationCommand) {
	appCmd = &discordgo.ApplicationCommand{
		Name:        cat.prefix + "more",
		Description: "use last search and get next result page",
		Options:     []*discordgo.ApplicationCommandOption{},
	}
	cmdFunc = func(i *discord.Interaction) {
		stat := cat.status.Get(i.ChannelID)
		if stat.searchFunc != cat.prefix+"search" {
			if err := i.SendInteractionResponseMessage(fmt.Sprintf("search \"%s\" not supported", stat.searchFunc)); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}
		if len(stat.result) == 0 || stat.lastQuery == "" {
			if err := i.SendInteractionResponseMessage("No search results available"); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}
		if cat.tryLock(i.ChannelID) == false {
			if err := i.SendInteractionResponseMessage("Please wait for the previous search to finish"); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}
		session := i.GetSession()
		channel, err := session.State.Channel(i.ChannelID)
		if err != nil {
			cat.logger.Error().Msgf("Error getting channel: %v", err)
			if err := i.SendChannelMessage(fmt.Sprintf("Error getting channel: %v", err)); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}
		filter := FilterFromChannelTopic(channel.Topic)

		if err := i.SendInteractionResponseMessage("Searching for more results"); err != nil {
			cat.logger.Error().Msgf("Error sending response: %v", err)
		}

		go func() {
			defer cat.unlock(i.ChannelID)

			var searchType SearchType
			var vector []float32

			var result *index.Result
			var sErr error
			if strings.HasPrefix(stat.lastQuery, "similar:") {
				cat.logger.Debug().Msgf("searching %s similarities for: %s", stat.lastSearchType, stat.lastQuery)
				result, sErr = cat.Search("", filter, vector, searchType, int64(len(stat.result)), stat.config.maxResults)
			} else {
				cat.logger.Debug().Msgf("searching for: %s", stat.lastQuery)
				result, sErr = cat.Search(stat.lastQuery, filter, stat.lastVector, stat.lastSearchType, int64(len(stat.result)), stat.config.maxResults)
			}
			if sErr != nil {
				cat.logger.Error().Msgf("Error searching: %v", sErr)
				if err := i.SendChannelMessage(fmt.Sprintf("Error searching: %v", sErr)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}

			embeds, err := cat.Result2MessageEmbed(result, stat)
			if err != nil {
				cat.logger.Error().Msgf("Error creating response: %v", err)
				if err := i.SendChannelMessage(fmt.Sprintf("Error creating response: %v", err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
			if stat.result == nil {
				stat.result = []*schema.UBSchema{}
			}
			for _, entry := range result.Docs {
				stat.result = append(stat.result, entry)
			}
			if err := i.SendChannelEmbeds(embeds); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
				if err := i.SendChannelMessage(fmt.Sprintf("Error sending response: %v", err)); err != nil {
					cat.logger.Error().Msgf("Error sending response: %v", err)
				}
				return
			}
		}()
	}
	return
}

func (cat *Catalog) CommandResultSize() (cmdFunc discord.CommandCreate, appCmd *discordgo.ApplicationCommand) {
	appCmd = &discordgo.ApplicationCommand{
		Name:        cat.prefix + "resultsize",
		Description: "number of items in search result set",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionInteger,
				Name:        "size",
				Description: "Size of search result set",
				Required:    true,
			},
		},
	}
	cmdFunc = func(i *discord.Interaction) {
		data := i.ApplicationCommandData()
		if len(data.Options) < 1 {
			if err := i.SendInteractionResponseMessage("Please provide result size"); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}
		size := data.Options[0].IntValue()
		if size < 1 || size > maxResultSize {
			if err := i.SendInteractionResponseMessage(fmt.Sprintf("Invalid result size %d. must be in (0,%d]", size, maxResultSize)); err != nil {
				cat.logger.Error().Msgf("Error sending response: %v", err)
			}
			return
		}
		stat := cat.status.Get(i.ChannelID)
		stat.config.maxResults = size

		if err := i.SendInteractionResponseMessage(fmt.Sprintf("Result size set to %d", size)); err != nil {
			cat.logger.Error().Msgf("Error sending response: %v", err)
		}
	}
	return
}

func (cat *Catalog) InitCommands(session *discord.Session) error {
	var cmdFunc discord.CommandCreate
	var appCmd *discordgo.ApplicationCommand

	cmdFunc, appCmd = cat.CommandResultSize()
	if err := session.ApplicationCommandCreate(cmdFunc, appCmd); err != nil {
		return errors.Wrap(err, "cannot create resultsize command")
	}

	cmdFunc, appCmd = cat.CommandMagic()
	if err := session.ApplicationCommandCreate(cmdFunc, appCmd); err != nil {
		return errors.Wrap(err, "cannot create magic command")
	}

	cmdFunc, appCmd = cat.CommandSearch()
	if err := session.ApplicationCommandCreate(cmdFunc, appCmd); err != nil {
		return errors.Wrap(err, "cannot create search command")
	}

	cmdFunc, appCmd = cat.CommandSearchKNN()
	if err := session.ApplicationCommandCreate(cmdFunc, appCmd); err != nil {
		return errors.Wrap(err, "cannot create search command")
	}

	cmdFunc, appCmd = cat.CommandSimilar()
	if err := session.ApplicationCommandCreate(cmdFunc, appCmd); err != nil {
		return errors.Wrap(err, "cannot create similar command")
	}

	cmdFunc, appCmd = cat.CommandSimilarKNN()
	if err := session.ApplicationCommandCreate(cmdFunc, appCmd); err != nil {
		return errors.Wrap(err, "cannot create similar command")
	}

	cmdFunc, appCmd = cat.CommandMore()
	if err := session.ApplicationCommandCreate(cmdFunc, appCmd); err != nil {
		return errors.Wrap(err, "cannot create more command")
	}

	cmdFunc, appCmd = cat.CommandText()
	if err := session.ApplicationCommandCreate(cmdFunc, appCmd); err != nil {
		return errors.Wrap(err, "cannot create text command")
	}
	return nil
}
