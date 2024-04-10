package main

import (
	"crypto/tls"
	"flag"
	"github.com/bwmarrin/discordgo"
	"github.com/dgraph-io/badger/v4"
	"github.com/elastic/elastic-transport-go/v8/elastictransport"
	"github.com/elastic/go-elasticsearch/v8"
	"github.com/je4/ub-bot/v2/pkg/catalogue"
	"github.com/je4/ub-bot/v2/pkg/discord"
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

var embeddingCacheFolder = flag.String("cache", "./embeddings", "folder to store embeddings")
var logLevel = flag.String("loglevel", "DEBUG", "log level")
var elasticURL = flag.String("elastic", "http://localhost:9200", "Elasticsearch URL")
var elasticIndex = flag.String("index", "", "Elasticsearch index")
var devMode = flag.Bool("dev", false, "Development mode")

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

	prefix := ""
	if *devMode {
		prefix = "dev-"
	}
	client := catalogue.NewCatalogue(elastic, *elasticIndex, db, openaiApiKey, prefix, logger)

	dSession, err := discord.NewSession(os.Getenv("DISCORD_TOKEN"), APP_ID, GUILD_ID, logger)
	if err != nil {
		panic(err)
	}

	if err := client.InitCommands(dSession); err != nil {
		panic(err)
	}

	dSession.Open()
	defer dSession.Close()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
	log.Println("Graceful shutdown")

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
