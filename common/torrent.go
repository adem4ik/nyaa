package common

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
	elastic "gopkg.in/olivere/elastic.v5"

	"github.com/NyaaPantsu/nyaa/config"
	"github.com/NyaaPantsu/nyaa/db"
	"github.com/NyaaPantsu/nyaa/model"
	"github.com/NyaaPantsu/nyaa/util/log"
)

// TorrentParam defines all parameters that can be provided when searching for a torrent
type TorrentParam struct {
	All       bool // True means ignore everything but Max and Offset
	Full      bool // True means load all members
	Order     bool // True means ascending
	Status    Status
	Sort      SortMode
	Category  Category
	Max       uint32
	Offset    uint32
	UserID    uint32
	TorrentID uint32
	FromID    uint32
	FromDate  string
	ToDate    string
	NotNull   string // csv
	Null      string // csv
	NameLike  string // csv
}

// FromRequest : parse a request in torrent param
// TODO Should probably return an error ?
func (p *TorrentParam) FromRequest(r *http.Request) {
	var err error

	nameLike := strings.TrimSpace(r.URL.Query().Get("q"))

	page := mux.Vars(r)["page"]
	pagenum, err := strconv.ParseUint(page, 10, 32)
	if err != nil {
		pagenum = 1
	}

	max, err := strconv.ParseUint(r.URL.Query().Get("max"), 10, 32)
	if err != nil {
		max = uint64(config.Conf.Navigation.TorrentsPerPage)
	} else if max > uint64(config.Conf.Navigation.MaxTorrentsPerPage) {
		max = uint64(config.Conf.Navigation.MaxTorrentsPerPage)
	}

	// FIXME 0 means no userId defined
	userID, err := strconv.ParseUint(r.URL.Query().Get("userID"), 10, 32)
	if err != nil {
		userID = 0
	}

	// FIXME 0 means no userId defined
	fromID, err := strconv.ParseUint(r.URL.Query().Get("fromID"), 10, 32)
	if err != nil {
		fromID = 0
	}

	var status Status
	status.Parse(r.URL.Query().Get("s"))

	p.FromDate = r.URL.Query().Get("fromDate")
	p.ToDate = r.URL.Query().Get("toDate")

	var category Category
	category.Parse(r.URL.Query().Get("c"))

	var sortMode SortMode
	sortMode.Parse(r.URL.Query().Get("sort"))

	ascending := false
	if r.URL.Query().Get("order") == "true" {
		ascending = true
	}

	p.NameLike = nameLike
	p.Offset = uint32(pagenum)
	p.Max = uint32(max)
	p.UserID = uint32(userID)
	// TODO Use All
	p.All = false
	// TODO Use Full
	p.Full = false
	p.Order = ascending
	p.Status = status
	p.Sort = sortMode
	p.Category = category
	// FIXME 0 means no TorrentId defined
	// Do we even need that ?
	p.TorrentID = 0
	// Needed to display result after a certain torrentID
	p.FromID = uint32(fromID)
}

// ToFilterQuery : Builds a query string with for es query string query defined here
// https://www.elastic.co/guide/en/elasticsearch/reference/current/query-dsl-query-string-query.html
func (p *TorrentParam) ToFilterQuery() string {
	// Don't set sub category unless main category is set
	query := ""
	if p.Category.IsMainSet() {
		query += "category:" + strconv.FormatInt(int64(p.Category.Main), 10)
		if p.Category.IsSubSet() {
			query += " sub_category:" + strconv.FormatInt(int64(p.Category.Sub), 10)
		}
	}

	if p.UserID != 0 {
		query += " uploader_id:" + strconv.FormatInt(int64(p.UserID), 10)
	}

	if p.Status != ShowAll {
		if p.Status != FilterRemakes {
			query += " status:" + p.Status.ToString()
		} else {
			/* From the old nyaa behavior, FilterRemake means everything BUT
			 * remakes
			 */
			query += " !status:" + p.Status.ToString()
		}
	}

	if p.FromID != 0 {
		query += " id:>" + strconv.FormatInt(int64(p.FromID), 10)
	}

	if p.FromDate != "" && p.ToDate != "" {
		query += " date: [" + p.FromDate + " " + p.ToDate + "]"
	} else if p.FromDate != "" {
		query += " date: [" + p.FromDate + " *]"
	} else if p.ToDate != "" {
		query += " date: [* " + p.ToDate + "]"
	}

	return query
}

// Find :
/* Uses elasticsearch to find the torrents based on TorrentParam
 * We decided to fetch only the ids from ES and then query these ids to the
 * database
 */
func (p *TorrentParam) Find(client *elastic.Client) (int64, []model.Torrent, error) {
	// TODO Why is it needed, what does it do ?
	ctx := context.Background()

	var query elastic.Query
	if p.NameLike == "" {
		query = elastic.NewMatchAllQuery()
	} else {
		query = elastic.NewSimpleQueryStringQuery(p.NameLike).
			Field("name").
			Analyzer(config.Conf.Search.ElasticsearchAnalyzer).
			DefaultOperator("AND")
	}

	fsc := elastic.NewFetchSourceContext(true).
		Include("id")

	// TODO Find a better way to keep in sync with mapping in ansible
	search := client.Search().
		Index(config.Conf.Search.ElasticsearchIndex).
		Query(query).
		Type(config.Conf.Search.ElasticsearchType).
		From(int((p.Offset-1)*p.Max)).
		Size(int(p.Max)).
		Sort(p.Sort.ToESField(), p.Order).
		Sort("_score", false). // Don't put _score before the field sort, it messes with the sorting
		FetchSourceContext(fsc)

	filterQueryString := p.ToFilterQuery()
	if filterQueryString != "" {
		filterQuery := elastic.NewQueryStringQuery(filterQueryString).
			DefaultOperator("AND")
		search = search.PostFilter(filterQuery)
	}

	result, err := search.Do(ctx)
	if err != nil {
		return 0, nil, err
	}

	log.Infof("Query '%s' took %d milliseconds.", p.NameLike, result.TookInMillis)
	log.Infof("Amount of results %d.", result.TotalHits())

	/* TODO Cleanup this giant mess
	 * The raw query is used because we need to preserve the order of the id's
	 * in the IN clause, so we can't just do
	 *      select * from torrents where torrent_id IN (list_of_ids)
	 * This query is said to work on postgres 9.4+
	 */
	{
		// Temporary struct to hold the id
		// INFO We are not using Hits.Id because the id in the index might not
		// correspond to the id in the database later on.
		type TId struct {
			Id uint
		}
		var tid TId
		var torrents []model.Torrent
		if len(result.Hits.Hits) > 0 {
			torrents = make([]model.Torrent, len(result.Hits.Hits))
			hits := result.Hits.Hits
			// Building a string of the form {id1,id2,id3}
			source, _ := hits[0].Source.MarshalJSON()
			json.Unmarshal(source, &tid)
			idsToString := "{" + strconv.FormatUint(uint64(tid.Id), 10)
			for _, t := range hits[1:] {
				source, _ = t.Source.MarshalJSON()
				json.Unmarshal(source, &tid)
				idsToString += "," + strconv.FormatUint(uint64(tid.Id), 10)
			}
			idsToString += "}"
			db.ORM.Raw("SELECT * FROM " + config.Conf.Models.TorrentsTableName +
				" JOIN unnest('" + idsToString + "'::int[]) " +
				" WITH ORDINALITY t(torrent_id, ord) USING (torrent_id) ORDER  BY t.ord").Find(&torrents)
		}
		return result.TotalHits(), torrents, nil
	}

}

// Clone : To clone a torrent params
func (p *TorrentParam) Clone() TorrentParam {
	return TorrentParam{
		Order:     p.Order,
		Status:    p.Status,
		Sort:      p.Sort,
		Category:  p.Category,
		Max:       p.Max,
		Offset:    p.Offset,
		UserID:    p.UserID,
		TorrentID: p.TorrentID,
		FromID:    p.FromID,
		FromDate:  p.FromDate,
		ToDate:    p.ToDate,
		NotNull:   p.NotNull,
		Null:      p.Null,
		NameLike:  p.NameLike,
	}
}
