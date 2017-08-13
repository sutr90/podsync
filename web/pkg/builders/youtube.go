package builders

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/BrianHicks/finch/duration"
	itunes "github.com/mxpv/podcast"
	"github.com/mxpv/podsync/web/pkg/api"
	"github.com/pkg/errors"
	"google.golang.org/api/youtube/v3"
)

const (
	maxYoutubeResults = 50
	hdBytesPerSecond  = 350000
	ldBytesPerSecond  = 100000
)

type apiKey string

func (key apiKey) Get() (string, string) {
	return "key", string(key)
}

type YouTubeBuilder struct {
	client *youtube.Service
	key    apiKey
}

func (yt *YouTubeBuilder) listChannels(linkType api.LinkType, id string) (*youtube.Channel, error) {
	req := yt.client.Channels.List("id,snippet,contentDetails")

	if linkType == api.Channel {
		req = req.Id(id)
	} else if linkType == api.User {
		req = req.ForUsername(id)
	} else {
		return nil, errors.New("unsupported link type")
	}

	resp, err := req.Do(yt.key)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to query channel")
	}

	if len(resp.Items) == 0 {
		return nil, errors.New("channel not found")
	}

	item := resp.Items[0]
	return item, nil
}

func (yt *YouTubeBuilder) listPlaylists(id, channelId string) (*youtube.Playlist, error) {
	req := yt.client.Playlists.List("id,snippet")

	if id != "" {
		req = req.Id(id)
	} else {
		req = req.ChannelId(channelId)
	}

	resp, err := req.Do(yt.key)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to query playlist")
	}

	if len(resp.Items) == 0 {
		return nil, errors.New("playlist not found")
	}

	item := resp.Items[0]
	return item, nil
}

func (yt *YouTubeBuilder) listPlaylistItems(itemId string, pageToken string) ([]*youtube.PlaylistItem, string, error) {
	req := yt.client.PlaylistItems.List("id,snippet").MaxResults(maxYoutubeResults).PlaylistId(itemId)
	if pageToken != "" {
		req = req.PageToken(pageToken)
	}

	resp, err := req.Do(yt.key)
	if err != nil {
		return nil, "", errors.Wrap(err, "failed to query playlist items")
	}

	return resp.Items, resp.NextPageToken, nil
}

func (yt *YouTubeBuilder) parseDate(s string) (time.Time, error) {
	date, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, errors.Wrapf(err, "failed to parse date: %s", s)
	}

	return date, nil
}

func (yt *YouTubeBuilder) selectThumbnail(snippet *youtube.ThumbnailDetails, quality api.Quality) string {
	// Use high resolution thumbnails for high quality mode
	// https://github.com/mxpv/Podsync/issues/14
	if quality == api.HighQuality {
		if snippet.Maxres != nil {
			return snippet.Maxres.Url
		}

		if snippet.High != nil {
			return snippet.High.Url
		}

		if snippet.Medium != nil {
			return snippet.Medium.Url
		}
	}

	return snippet.Default.Url
}

func (yt *YouTubeBuilder) queryFeed(feed *api.Feed) (*itunes.Podcast, string, error) {
	now := time.Now()

	if feed.LinkType == api.Channel || feed.LinkType == api.User {
		channel, err := yt.listChannels(feed.LinkType, feed.ItemId)
		if err != nil {
			return nil, "", errors.Wrap(err, "failed to query channel")
		}

		itemId := channel.ContentDetails.RelatedPlaylists.Uploads

		link := ""
		if feed.LinkType == api.Channel {
			link = fmt.Sprintf("https://youtube.com/channel/%s", itemId)
		} else {
			link = fmt.Sprintf("https://youtube.com/user/%s", itemId)
		}

		pubDate, err := yt.parseDate(channel.Snippet.PublishedAt)
		if err != nil {
			return nil, "", err
		}

		title := channel.Snippet.Title

		podcast := itunes.New(title, link, channel.Snippet.Description, &pubDate, &now)
		podcast.Generator = podsyncGenerator

		podcast.AddSubTitle(title)
		podcast.AddCategory(defaultCategory, nil)
		podcast.AddImage(yt.selectThumbnail(channel.Snippet.Thumbnails, feed.Quality))

		return &podcast, itemId, nil
	}

	if feed.LinkType == api.Playlist {
		playlist, err := yt.listPlaylists(feed.ItemId, "")
		if err != nil {
			return nil, "", errors.Wrap(err, "failed to query playlist")
		}

		link := fmt.Sprintf("https://youtube.com/playlist?list=%s", playlist.Id)

		snippet := playlist.Snippet

		pubDate, err := yt.parseDate(snippet.PublishedAt)
		if err != nil {
			return nil, "", err
		}

		title := fmt.Sprintf("%s: %s", snippet.ChannelTitle, snippet.Title)

		podcast := itunes.New(title, link, snippet.Description, &pubDate, &now)
		podcast.Generator = podsyncGenerator

		podcast.AddSubTitle(title)
		podcast.AddCategory(defaultCategory, nil)
		podcast.AddImage(yt.selectThumbnail(snippet.Thumbnails, feed.Quality))

		return &podcast, playlist.Id, nil
	}

	return nil, "", errors.New("unsupported link format")
}

func (yt *YouTubeBuilder) getVideoSize(definition string, duration int64, fmt api.Format) int64 {
	// Video size information requires 1 additional call for each video (1 feed = 50 videos = 50 calls),
	// which is too expensive, so get approximated size depending on duration and definition params
	var size int64 = 0

	if definition == "hd" {
		size = duration * hdBytesPerSecond
	} else {
		size = duration * ldBytesPerSecond
	}

	// Some podcasts are coming in with exactly double the actual runtime and with the second half just silence.
	// https://github.com/mxpv/Podsync/issues/6
	if fmt == api.AudioFormat {
		size /= 2
	}

	return size
}

func (yt *YouTubeBuilder) queryVideoDescriptions(ids []string, feed *api.Feed, podcast *itunes.Podcast) error {
	req, err := yt.client.Videos.List("id,snippet,contentDetails").Id(strings.Join(ids, ",")).Do(yt.key)
	if err != nil {
		return errors.Wrap(err, "failed to query video descriptions")
	}

	for _, video := range req.Items {
		snippet := video.Snippet

		item := itunes.Item{}

		item.GUID = video.Id
		item.Link = fmt.Sprintf("https://youtube.com/watch?v=%s", video.Id)
		item.Title = snippet.Title
		item.Description = snippet.Description
		item.ISubtitle = snippet.Title

		// Select thumbnail

		item.AddImage(yt.selectThumbnail(snippet.Thumbnails, feed.Quality))

		// Parse publication date

		pubDate, err := yt.parseDate(snippet.PublishedAt)
		if err != nil {
			return errors.Wrapf(err, "failed to parse video publish date: %s", snippet.PublishedAt)
		}

		item.AddPubDate(&pubDate)

		// Parse duration

		d, err := duration.FromString(video.ContentDetails.Duration)
		if err != nil {
			return errors.Wrapf(err, "failed to parse duration %s", video.ContentDetails.Duration)
		}

		seconds := int64(d.ToDuration().Seconds())
		item.AddDuration(seconds)

		// Add download links

		size := yt.getVideoSize(video.ContentDetails.Definition, seconds, feed.Format)
		item.AddEnclosure(makeEnclosure(feed, video.Id, size))

		_, err = podcast.AddItem(item)
		if err != nil {
			return errors.Wrapf(err, "failed to add item to podcast (id '%s')", video.Id)
		}
	}

	return nil
}

func (yt *YouTubeBuilder) queryItems(itemId string, feed *api.Feed, podcast *itunes.Podcast) error {
	pageToken := ""
	count := 0

	for {
		items, pageToken, err := yt.listPlaylistItems(itemId, pageToken)
		if err != nil {
			return err
		}

		if len(items) == 0 {
			return nil
		}

		// Extract video ids
		ids := make([]string, len(items))
		for index, item := range items {
			ids[index] = item.Snippet.ResourceId.VideoId
			count++
		}

		// Query video descriptions from the list of ids
		if err := yt.queryVideoDescriptions(ids, feed, podcast); err != nil {
			return err
		}

		if count >= feed.PageSize || pageToken == "" {
			return nil
		}
	}
}

func (yt *YouTubeBuilder) Build(feed *api.Feed) (*itunes.Podcast, error) {

	// Query general information about feed (title, description, lang, etc)

	podcast, itemId, err := yt.queryFeed(feed)
	if err != nil {
		return nil, err
	}

	// Get video descriptions

	if err := yt.queryItems(itemId, feed, podcast); err != nil {
		return nil, err
	}

	return podcast, nil
}

func NewYouTubeBuilder(key string) (*YouTubeBuilder, error) {
	yt, err := youtube.New(&http.Client{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to create youtube client")
	}

	return &YouTubeBuilder{client: yt, key: apiKey(key)}, nil
}
