package main

import (
	"context"
	"emperror.dev/errors"
	"github.com/dgraph-io/badger/v4"
	"github.com/elastic/go-elasticsearch/v8"
	"github.com/je4/ubcat/v2/pkg/index"
	"github.com/je4/ubcat/v2/pkg/schema"
	"github.com/je4/utils/v2/pkg/openai"
	"github.com/je4/utils/v2/pkg/zLogger"
	oai "github.com/sashabaranov/go-openai"
)

type SearchType int

const (
	SearchTypeSimple SearchType = iota
	SearchTypeEmbeddingMARC
	SearchTypeEmbeddingProse
	SearchTypeEmbeddingJSON
)

func NewCatalogue(elastic *elasticsearch.TypedClient, elasticIndex string, badgerDB *badger.DB, openaiApiKey string, logger zLogger.ZLogger) *Catalog {
	kvBadger := openai.NewKVBadger(badgerDB)
	client := openai.NewClientV2(openaiApiKey, kvBadger, logger)
	cat := &Catalog{
		ubClient: index.NewClient(elasticIndex, elastic),
		client:   client,
		logger:   logger,
	}
	return cat
}

type Catalog struct {
	ubClient *index.Client
	client   *openai.ClientV2
	logger   zLogger.ZLogger
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

func (cat *Catalog) Search(queryString string, embedding []float32, searchType SearchType, from, num int) (map[string]*schema.UBSchema, error) {
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
	res, err := cat.ubClient.Search(context.Background(), queryString, vectorMarc, vectorJSON, vectorProse, from, num)
	if err != nil {
		return nil, errors.Wrap(err, "cannot search")
	}
	return res, nil
}
