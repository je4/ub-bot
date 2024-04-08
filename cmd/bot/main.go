package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"github.com/bwmarrin/discordgo"
	"github.com/dgraph-io/badger/v4"
	"github.com/elastic/elastic-transport-go/v8/elastictransport"
	"github.com/elastic/go-elasticsearch/v8"
	"github.com/je4/ubcat/v2/pkg/schema"
	"github.com/je4/utils/v2/pkg/zLogger"
	"github.com/rs/zerolog"
	"go.elastic.co/apm/module/apmelasticsearch"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"
)

const GUILD_ID = "1222591253255032913"
const APP_ID = "1222592521310437446"

const defaultResultSize = 10
const maxResultSize = 100

var (
	commands = []discordgo.ApplicationCommand{
		{
			Name:        "ping",
			Description: "Replies with Pong!",
		},
	}

	commandsHandlers = map[string]func(s *discordgo.Session, i *discordgo.InteractionCreate){
		"ping": func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "Pong!",
				},
			})
		},
	}
)

var embeddingCacheFolder = flag.String("cache", "./embeddings", "folder to store embeddings")
var logLevel = flag.String("loglevel", "DEBUG", "log level")
var elasticURL = flag.String("elastic", "http://localhost:9200", "Elasticsearch URL")
var elasticIndex = flag.String("index", "", "Elasticsearch index")

func main() {
	flag.Parse()

	openaiApiKey := os.Getenv("OPENAI_API_KEY")
	elasticApiKey := os.Getenv("ELASTIC_API_KEY")

	var out io.Writer = os.Stderr

	output := zerolog.ConsoleWriter{Out: out, TimeFormat: time.RFC3339}
	_logger := zerolog.New(output).With().Timestamp().Logger()
	switch strings.ToUpper(*logLevel) {
	case "DEBUG":
		_logger = _logger.Level(zerolog.DebugLevel)
	case "INFO":
		_logger = _logger.Level(zerolog.InfoLevel)
	case "WARN":
		_logger = _logger.Level(zerolog.WarnLevel)
	case "ERROR":
		_logger = _logger.Level(zerolog.ErrorLevel)
	case "FATAL":
		_logger = _logger.Level(zerolog.FatalLevel)
	case "PANIC":
		_logger = _logger.Level(zerolog.PanicLevel)
	default:
		_logger = _logger.Level(zerolog.DebugLevel)
	}
	var logger zLogger.ZLogger = &_logger

	if fi, err := os.Stat(*embeddingCacheFolder); err != nil {
		logger.Fatal().Msgf("Cannot access folder %s: %v", *embeddingCacheFolder, err)
	} else if !fi.IsDir() {
		logger.Fatal().Msgf("Path %s is not a directory", *embeddingCacheFolder)
	}

	db, err := badger.Open(badger.DefaultOptions(*embeddingCacheFolder))
	if err != nil {
		logger.Fatal().Msgf("Cannot open badger db: %v", err)
	}

	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true,
	}
	elasticConfig := elasticsearch.Config{
		APIKey:    elasticApiKey,
		Addresses: []string{*elasticURL},

		// Retry on 429 TooManyRequests statuses
		//
		RetryOnStatus: []int{502, 503, 504, 429},

		// Retry up to 5 attempts
		//
		MaxRetries: 5,

		Logger: &elastictransport.ColorLogger{Output: os.Stdout},
		//		Transport: doer,
		Transport: apmelasticsearch.WrapRoundTripper(http.DefaultTransport),
	}

	elastic, err := elasticsearch.NewTypedClient(elasticConfig)
	if err != nil {
		logger.Fatal().Err(err)
	}

	client := NewCatalogue(elastic, *elasticIndex, db, openaiApiKey, logger)

	var resultCache = map[string][]*schema.UBSchema{}
	type channelConfig struct {
		maxResults int
	}
	var config = map[string]channelConfig{}

	commands = append(commands, discordgo.ApplicationCommand{
		Name:        "resultsize",
		Description: "number of items in search result set",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionInteger,
				Name:        "size",
				Description: "Size of search result set",
				Required:    true,
			},
		},
	})
	commandsHandlers["resultsize"] = func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		// get the search query from the user
		data := i.ApplicationCommandData()
		if len(data.Options) < 1 {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "Please provide result size",
				},
			})
			return
		}
		size := data.Options[0].IntValue()
		if size < 1 || size > maxResultSize {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: fmt.Sprintf("Invalid result size %d. must be in (0,%d]", size, maxResultSize),
				},
			})
			return
		}
		config[i.ChannelID] = channelConfig{
			maxResults: int(size),
		}
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: fmt.Sprintf("Result size set to %d", size),
			},
		})
	}

	commands = append(commands, discordgo.ApplicationCommand{
		Name:        "magic",
		Description: "Magic search",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "query",
				Description: "Query to do magic with",
				Required:    true,
			},
		},
	})
	commandsHandlers["magic"] = func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		data := i.ApplicationCommandData()
		logger.Debug().Msgf("command name: %s", data.Name)
		if len(data.Options) < 1 {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "Please provide query",
				},
			})
			return
		}
		query := data.Options[0].StringValue()
		logger.Debug().Msgf("magic query: %s", query)
		newQuery, err := client.Query2Embedding(query)
		if err != nil {
			logger.Error().Msgf("Error converting query: %v", err)
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: fmt.Sprintf("Error converting query: %v", err),
				},
			})
			return
		}
		logger.Debug().Msgf("new query: %s", newQuery)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: newQuery,
			},
		})
	}

	commands = append(commands, discordgo.ApplicationCommand{
		Name:        "search",
		Description: "Search the catalogue",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type: discordgo.ApplicationCommandOptionString,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{
						Name:  "Simple Elastic Query",
						Value: "simple",
					},
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
				Name:        "query",
				Description: "Query to ask for",
				Required:    true,
			},
		},
	})
	commandsHandlers["search"] = func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		// get the search query from the user
		data := i.ApplicationCommandData()
		logger.Debug().Msgf("command name: %s", data.Name)
		if len(data.Options) < 2 {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "Please provide search type and query",
				},
			})
			return
		}
		sType := data.Options[0].StringValue()
		query := data.Options[1].StringValue()
		var searchType SearchType
		var embedding []float32
		var err error
		switch sType {
		case "marc":
			embedding, err = client.GetEmbedding(query)
			searchType = SearchTypeEmbeddingMARC
		case "prose":
			embedding, err = client.GetEmbedding(query)
			searchType = SearchTypeEmbeddingProse
		case "json":
			embedding, err = client.GetEmbedding(query)
			searchType = SearchTypeEmbeddingJSON
		case "simple":
			searchType = SearchTypeSimple
		default:
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: fmt.Sprintf("Unknown search type %s", sType),
				},
			})
			return
		}
		if err != nil {
			logger.Error().Msgf("Error getting embedding: %v", err)
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: fmt.Sprintf("Error getting embedding: %v", err),
				},
			})
			return
		}

		cfg, ok := config[i.ChannelID]
		if !ok {
			cfg = channelConfig{
				maxResults: defaultResultSize,
			}
			config[i.ChannelID] = cfg
		}

		result, err := client.Search(query, embedding, searchType, 0, cfg.maxResults)
		if err != nil {
			logger.Error().Msgf("Error searching: %v", err)
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: fmt.Sprintf("Error searching: %v", err),
				},
			})
			return
		}
		content := fmt.Sprintf("searching %s for: %s\n", sType, query)

		resultCache[i.ChannelID] = []*schema.UBSchema{}
		var key int
		for _, entry := range result {
			resultCache[i.ChannelID] = append(resultCache[i.ChannelID], entry)
			content += fmt.Sprintf("% 2d: %s\n", key, entry.GetTitle())
			key++
		}

		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: content,
			},
		})
	}

	commands = append(commands, discordgo.ApplicationCommand{
		Name:        "similar",
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
				Type:        discordgo.ApplicationCommandOptionInteger,
				Name:        "resultid",
				Description: "Result ID from previous search",
				Required:    true,
			},
		},
	})
	commandsHandlers["similar"] = func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		data := i.ApplicationCommandData()
		if len(data.Options) < 2 {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "Please provide search type and result ID",
				},
			})
			return
		}
		sType := data.Options[0].StringValue()
		resultID := data.Options[1].IntValue()
		rc, ok := resultCache[i.ChannelID]
		if !ok {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "No search results available",
				},
			})
			return
		}
		if resultID < 0 || int(resultID) >= len(rc) {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "Invalid result ID",
				},
			})
			return
		}
		lastResult := rc[resultID]
		var searchType SearchType
		switch sType {
		case "marc":
			searchType = SearchTypeEmbeddingMARC
		case "prose":
			searchType = SearchTypeEmbeddingProse
		case "json":
			searchType = SearchTypeEmbeddingJSON
		default:
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: fmt.Sprintf("Unknown search type %s", sType),
				},
			})
			return
		}

		cfg, ok := config[i.ChannelID]
		if !ok {
			cfg = channelConfig{
				maxResults: defaultResultSize,
			}
			config[i.ChannelID] = cfg
		}

		logger.Debug().Msgf("searching %s similatities for: %s", sType, lastResult.GetTitle())
		result, err := client.Search("", lastResult.EmbeddingMarc, searchType, 0, cfg.maxResults)
		if err != nil {
			logger.Error().Msgf("Error searching: %v", err)
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: fmt.Sprintf("Error searching: %v", err),
				},
			})
			return
		}
		content := fmt.Sprintf("searching %s vector for: %s\n", sType, lastResult.GetTitle())
		resultCache[i.ChannelID] = []*schema.UBSchema{}
		var key int
		for _, entry := range result {
			resultCache[i.ChannelID] = append(resultCache[i.ChannelID], entry)
			content += fmt.Sprintf("% 2d: %s\n", key, entry.GetTitle())
			key++
		}
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: content,
			},
		})
	}

	discord, err := discordgo.New("Bot " + os.Getenv("DISCORD_TOKEN"))
	if err != nil {
		panic(err)
	}
	discord.AddHandler(newMessage)
	discord.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		logger.Info().Msg("Bot is up!")
		for _, guild := range r.Guilds {
			logger.Info().Msgf("Guild: %s\n", guild.ID)
		}
	})

	discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if h, ok := commandsHandlers[i.ApplicationCommandData().Name]; ok {
			h(s, i)
		}
	})

	cmdIDs := make(map[string]string, len(commands))

	for _, cmd := range commands {
		log.Printf("Creating command %q\n", cmd.Name)
		rcmd, err := discord.ApplicationCommandCreate(APP_ID, GUILD_ID, &cmd)
		if err != nil {
			log.Fatalf("Cannot create slash command %q: %v", cmd.Name, err)
		}

		cmdIDs[rcmd.ID] = rcmd.Name

	}
	u, err := discord.User("@me")
	if err != nil {
		fmt.Println("Failed getting current User:", err)
		return
	}
	logger.Info().Msgf("Bot user: %s", u.Username)

	discord.Open()
	defer discord.Close() // close session, after function termination

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
	log.Println("Graceful shutdown")

	for id, name := range cmdIDs {
		log.Printf("Deleting command %q\n", name)
		err := discord.ApplicationCommandDelete(APP_ID, GUILD_ID, id)
		if err != nil {
			log.Fatalf("Cannot delete slash command %q: %v", name, err)
		}
	}
}

func newMessage(discord *discordgo.Session, message *discordgo.MessageCreate) {
	/* prevent bot responding to its own message
	this is achived by looking into the message author id
	if message.author.id is same as bot.author.id then just return
	*/
	if message.Author.ID == discord.State.User.ID {
		return
	}
	log.Printf("%s: %s", message.Author.Username, message.Content)

	// respond to user message if it contains `!help` or `!bye`
	switch {
	case strings.Contains(message.Content, "!help"):
		discord.ChannelMessageSend(message.ChannelID, "Hello World😃")
	case strings.Contains(message.Content, "!bye"):
		discord.ChannelMessageSend(message.ChannelID, "Good Bye👋")
		// add more cases if required
	}

}
