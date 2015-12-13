package mpd

import (
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	player "../"
	"../../util"
	"github.com/polyfloyd/gompd/mpd"
)

const URI_SCHEMA = "mpd://"

// This type allows MPD connections to be reused. It's run method will keep the
// connection alive by periodicaly sending a ping. The client expires after
// nothing gets sent over the reset channel for some time or an error occurs
// when sending a ping. When this happens, the client is set to nil and the
// reset channel is closed.
type reusableClient struct {
	sync.Mutex
	client *mpd.Client
	reset  chan struct{}
}

func (rc *reusableClient) run(expireAfter time.Duration) {
	pinger := time.NewTicker(time.Second * 4)
	defer pinger.Stop()
	defer close(rc.reset)

	expire := time.After(expireAfter)
	for {
		select {
		case <-pinger.C:
			rc.Lock()
			if err := rc.client.Ping(); err != nil {
				rc.client.Close()
				rc.client = nil
				rc.Unlock()
				return
			}
			rc.Unlock()
		case <-expire:
			rc.Lock()
			rc.client.Close()
			rc.client = nil
			rc.Unlock()
			return
		case <-rc.reset:
			expire = time.After(expireAfter)
		}
	}
}

type Player struct {
	util.Emitter

	clientPool chan *reusableClient

	network, address string
	passwd           string

	playlist []player.PlaylistTrack
	// The playlist mutex should not be locked within a withMpd() callback as
	// it may result in deadlocks.
	playlistLock sync.Mutex

	// Sometimes, the volume returned by MPD is invalid, so we have to take
	// care of that ourselves.
	lastVolume float32

	// We use this value to determine wether the currently playing track has
	// changed so its play count can be updated.
	lastTrack string
}

func Connect(network, address string, mpdPassword *string) (*Player, error) {
	var passwd string
	if mpdPassword != nil {
		passwd = *mpdPassword
	} else {
		passwd = ""
	}

	player := &Player{
		Emitter:  util.Emitter{Release: time.Millisecond * 100},
		playlist: []player.PlaylistTrack{},
		network:  network,
		address:  address,
		passwd:   passwd,

		// NOTE: MPD supports up to 10 concurrent connections by default. When
		// this number is reached and ANYTHING tries to connect, the connection
		// rudely closed.
		clientPool: make(chan *reusableClient, 6),
	}

	for i := 0; i < cap(player.clientPool); i++ {
		reClient, err := player.newClient()
		if err != nil {
			return nil, err
		}
		player.clientPool <- reClient
	}

	go player.eventLoop()
	go player.mainLoop()
	return player, nil
}

func (pl *Player) newClient() (*reusableClient, error) {
	client, err := mpd.DialAuthenticated(pl.network, pl.address, pl.passwd)
	if err != nil {
		return nil, fmt.Errorf("Error connecting to MPD: %v", err)
	}

	rc := &reusableClient{
		client: client,
		reset:  make(chan struct{}),
	}
	go rc.run(time.Second * 30)
	return rc, nil
}

func (pl *Player) withMpd(fn func(*mpd.Client) error) error {
	reClient := <-pl.clientPool
	reClient.Lock()
	if reClient.client == nil {
		reClient.Unlock()
		var err error
		if reClient, err = pl.newClient(); err != nil {
			return err
		}
		reClient.Lock()
	}

	defer func() {
		reClient.Unlock()
		pl.clientPool <- reClient
	}()
	reClient.reset <- struct{}{}
	return fn(reClient.client)
}

func (pl *Player) eventLoop() {
	for {
		watcher, err := mpd.NewWatcher(pl.network, pl.address, pl.passwd)
		if err != nil {
			// Limit the number of reconnection attempts to one per second.
			time.Sleep(time.Second)
			continue
		}
		defer watcher.Close()
		pl.Emit("availability")

	loop:
		for {
			select {
			case event := <-watcher.Event:
				pl.Emit("mpd-" + event)
			case <-watcher.Error:
				pl.Emit("availability")
				break loop
			}
		}
	}
}

func (pl *Player) mainLoop() {
	listener := pl.Listen()
	defer pl.Unlisten(listener)
	listener <- "mpd-playlist" // Bootstrap the cycle

	for {
		switch event := <-listener; event {
		case "mpd-player":
			pl.Emit("playstate")
			pl.Emit("progress")
			fallthrough

		case "mpd-playlist":
			pl.playlistLock.Lock()
			err := pl.withMpd(func(mpdc *mpd.Client) error {
				// Check whether the playlist help by the player is in sync and
				// updates it if it is not.
				if inSync, err := pl.playlistMatchesServer(mpdc, pl.playlist); err != nil {
					return err
				} else if inSync {
					return nil
				}
				if err := pl.removePlayedTracks(mpdc); err != nil {
					return err
				}

				newPlistIds, err := pl.loadPlaylist(mpdc)
				if err != nil {
					return err
				}
				newPlist := player.InterpolatePlaylistMeta(pl.playlist, newPlistIds)
				if err != nil {
					return err
				}

				pl.playlist = newPlist
				pl.Emit("playlist")
				if len(newPlist) == 0 {
					pl.Emit("playlist-end")
				}
				return nil
			})
			pl.playlistLock.Unlock()
			if err != nil {
				log.Println(err)
			}

		case "playlist":
			pl.playlistLock.Lock()
			err := pl.withMpd(func(mpdc *mpd.Client) error {
				if len(pl.playlist) > 0 {
					cur := pl.playlist[0]
					if pl.lastTrack != "" && pl.lastTrack != cur.TrackUri() {
						// NOTE: If one track is followed by another track with the same
						// ID, this block will not be executed, leaving the playcount
						// unchanged.

						// Seek using the progress attr when the track starts playing.
						if cur.Progress != 0 {
							if err := pl.Seek(cur.Progress); err != nil {
								return err
							}
						}

						if err := incrementPlayCount(cur.TrackUri(), mpdc); err != nil {
							return err
						}
					}
					pl.lastTrack = cur.TrackUri()
				}
				return nil
			})
			pl.playlistLock.Unlock()
			if err != nil {
				log.Println(err)
			}

		case "mpd-mixer":
			pl.Emit("volume")

		case "mpd-update":
			err := pl.withMpd(func(mpdc *mpd.Client) error {
				status, err := mpdc.Status()
				if err != nil {
					return err
				}
				if _, ok := status["updating_db"]; !ok {
					pl.Emit("tracks")
				}
				return nil
			})
			if err != nil {
				log.Println(err)
			}
		}
	}
}

func (pl *Player) removePlayedTracks(mpdc *mpd.Client) error {
	status, err := mpdc.Status()
	if err != nil {
		return err
	}
	if songIndex, _ := statusAttrInt(status, "song"); songIndex > 0 {
		return mpdc.Delete(0, songIndex)
	}
	return nil
}

// Checks wether the argument playlist is equal to the playlist stored by the
// MPD server.
func (pl *Player) playlistMatchesServer(mpdc *mpd.Client, plist []player.PlaylistTrack) (bool, error) {
	songs, err := mpdc.PlaylistInfo(-1, -1)
	if err != nil {
		return false, err
	}

	if len(plist) == len(songs) {
		for i, song := range songs {
			if mpdToUri(song["file"]) != plist[i].TrackUri() {
				return false, nil
			}
		}
		return true, nil
	}
	return false, nil
}

// Gets the MPD server's playlist and interpolates the Trollibox metadata from
// the specified playlist.
func (pl *Player) loadPlaylist(mpdc *mpd.Client) ([]player.TrackIdentity, error) {
	songs, err := mpdc.PlaylistInfo(-1, -1)
	if err != nil {
		return nil, err
	}
	tracks := make([]player.TrackIdentity, len(songs))
	for i, song := range songs {
		tracks[i] = player.Track{Uri: mpdToUri(song["file"])}
	}
	return tracks, nil
}

// Initializes a track from an MPD hash. The hash should be gotten using
// ListAllInfo().
//
// ListAllInfo() and ListInfo() look very much the same but they don't return
// the same thing. Who the fuck thought it was a good idea to mix capitals and
// lowercase?!
func (pl *Player) trackFromMpdSong(mpdc *mpd.Client, song *mpd.Attrs, track *player.Track) {
	if _, ok := (*song)["directory"]; ok {
		panic("Tried to read a directory as local file")
	}

	track.Uri = mpdToUri((*song)["file"])
	track.Artist = (*song)["Artist"]
	track.Title = (*song)["Title"]
	track.Genre = (*song)["Genre"]
	track.Album = (*song)["Album"]
	track.AlbumArtist = (*song)["AlbumArtist"]
	track.AlbumDisc = (*song)["Disc"]
	track.AlbumTrack = (*song)["Track"]

	strNum, _ := mpdc.StickerGet((*song)["file"], "image-nchunks")
	_, err := strconv.ParseInt(strNum, 10, 32)
	track.HasArt = err == nil

	if timeStr := (*song)["Time"]; timeStr != "" {
		if duration, err := strconv.ParseInt(timeStr, 10, 32); err != nil {
			panic(err)
		} else {
			track.Duration = time.Duration(duration) * time.Second
		}
	}

	player.InterpolateMissingFields(track)
}

func (pl *Player) Tracks() ([]player.Track, error) {
	var tracks []player.Track
	err := pl.withMpd(func(mpdc *mpd.Client) error {
		songs, err := mpdc.ListAllInfo("/")
		if err != nil {
			return err
		}

		numDirs := 0
		tracks = make([]player.Track, len(songs))
		for i, song := range songs {
			if _, ok := song["directory"]; ok {
				numDirs++
			} else {
				pl.trackFromMpdSong(mpdc, &song, &tracks[i-numDirs])
			}
		}
		tracks = tracks[:len(tracks)-numDirs]
		return nil
	})
	return tracks, err
}

func (pl *Player) TrackInfo(identities ...player.TrackIdentity) ([]player.Track, error) {
	pl.playlistLock.Lock()
	currentTrackUri := ""
	if len(pl.playlist) > 0 {
		currentTrackUri = pl.playlist[0].TrackUri()
	}
	pl.playlistLock.Unlock()

	var tracks []player.Track
	err := pl.withMpd(func(mpdc *mpd.Client) error {
		songs := make([]mpd.Attrs, len(identities))
		for i, id := range identities {
			uri := id.TrackUri()
			if strings.HasPrefix(uri, URI_SCHEMA) {
				s, err := mpdc.ListAllInfo(uriToMpd(uri))
				if err != nil {
					return fmt.Errorf("Unable to get info about %v: %v", uri, err)
				}
				if len(s) > 0 {
					songs[i] = s[0]
				}
			} else if ok, _ := regexp.MatchString("https?:\\/\\/", uri); ok && currentTrackUri == uri {
				song, err := mpdc.CurrentSong()
				if err != nil {
					return fmt.Errorf("Unable to get info about %v: %v", uri, err)
				}
				songs[i] = song
				songs[i]["Album"] = song["Name"]
			}
		}

		numDirs := 0
		tracks = make([]player.Track, len(songs))
		for i, song := range songs {
			if _, ok := song["directory"]; ok {
				numDirs++
			} else if song != nil {
				pl.trackFromMpdSong(mpdc, &song, &tracks[i-numDirs])
			}
		}
		tracks = tracks[:len(tracks)-numDirs]
		return nil
	})
	return tracks, err
}

func (pl *Player) Playlist() ([]player.PlaylistTrack, error) {
	pl.playlistLock.Lock()
	defer pl.playlistLock.Unlock()
	if len(pl.playlist) == 0 {
		return pl.playlist, nil
	}

	plist := make([]player.PlaylistTrack, len(pl.playlist))
	copy(plist, pl.playlist)

	// Update the progress attribute of the currently playing track.
	err := pl.withMpd(func(mpdc *mpd.Client) error {
		status, err := mpdc.Status()
		if err != nil {
			return err
		}

		progressf, _ := strconv.ParseFloat(status["elapsed"], 32)
		plist[0].Progress = time.Duration(progressf) * time.Second
		return nil
	})
	if err != nil {
		return nil, err
	}
	return plist, nil
}

func (pl *Player) SetPlaylist(plist []player.PlaylistTrack) error {
	// A lot of mpd-update events will be emitted during this process. But the
	// mainLoop will not abserve a half playlist if we lock the playlist's
	// mutex.
	pl.playlistLock.Lock()
	defer pl.playlistLock.Unlock()
	return pl.withMpd(func(mpdc *mpd.Client) error {
		songs, err := mpdc.PlaylistInfo(-1, -1)
		if err != nil {
			return err
		}

		// Figure out how many tracks at the beginning of the playlist are unchanged.
		delStart := 0
		for len(songs) > delStart && len(plist) > delStart && uriToMpd(plist[delStart].TrackUri()) == songs[delStart]["file"] {
			delStart++
		}
		if delStart != len(songs) {
			// Clear the part of the playlist that does not match the new playlist.
			if err := mpdc.Delete(delStart, len(songs)); err != nil {
				return err
			}
		}

		// Queue the new tracks.
		cmd := mpdc.BeginCommandList()
		for _, track := range plist[delStart:] {
			cmd.Add(uriToMpd(track.TrackUri()))
		}
		if err := cmd.End(); err != nil {
			return err
		}

		pl.playlist = plist
		pl.Emit("playlist")

		if len(pl.playlist) == 0 {
			pl.Emit("playlist-end")
		} else {
			// Start playing if we were not.
			if state, err := pl.State(); err != nil {
				return err
			} else if len(plist) > 0 && state != player.PlayStatePlaying {
				return pl.SetState(player.PlayStatePlaying)
			}
		}
		return nil
	})
}

func (player *Player) Seek(progress time.Duration) error {
	return player.withMpd(func(mpdc *mpd.Client) error {
		status, err := mpdc.Status()
		if err != nil {
			return err
		}

		id, ok := statusAttrInt(status, "songid")
		if !ok {
			// No track is currently being played.
			return nil
		}
		return mpdc.SeekID(id, int(progress/time.Second))
	})
}

func (pl *Player) State() (player.PlayState, error) {
	var state player.PlayState
	err := pl.withMpd(func(mpdc *mpd.Client) error {
		status, err := mpdc.Status()
		if err != nil {
			return err
		}

		state = map[string]player.PlayState{
			"play":  player.PlayStatePlaying,
			"pause": player.PlayStatePaused,
			"stop":  player.PlayStateStopped,
		}[status["state"]]
		return nil
	})
	return state, err
}

func (pl *Player) SetState(state player.PlayState) error {
	return pl.withMpd(func(mpdc *mpd.Client) error {
		switch state {
		case player.PlayStatePaused:
			return mpdc.Pause(true)
		case player.PlayStatePlaying:
			status, err := mpdc.Status()
			if err != nil {
				return err
			}

			// Don't attempt to start playback, just immediately end the
			// playlist.
			if status["playlistlength"] == "0" {
				pl.Emit("playlist-end")
				return nil
			}

			if status["state"] == "stop" {
				return mpdc.Play(0)
			} else {
				return mpdc.Pause(false)
			}
		case player.PlayStateStopped:
			return mpdc.Stop()
		default:
			return fmt.Errorf("Unknown play state %v", state)
		}
	})
}

func (pl *Player) Volume() (float32, error) {
	var vol float32
	err := pl.withMpd(func(mpdc *mpd.Client) error {
		status, err := mpdc.Status()
		if err != nil {
			return err
		}

		volInt, ok := statusAttrInt(status, "volume")
		if !ok {
			// Volume should always be present.
			return fmt.Errorf("No volume property is present in the MPD status")
		}

		vol = float32(volInt) / 100
		if vol < 0 {
			// Happens sometimes when nothing is playing.
			vol = pl.lastVolume
		}
		return nil
	})
	return vol, err
}

func (player *Player) SetVolume(vol float32) error {
	return player.withMpd(func(mpdc *mpd.Client) error {
		if vol > 1 {
			vol = 1
		} else if vol < 0 {
			vol = 0
		}

		player.lastVolume = vol
		return mpdc.SetVolume(int(vol * 100))
	})
}

func (pl *Player) Available() bool {
	return pl.withMpd(func(mpdc *mpd.Client) error { return mpdc.Ping() }) == nil
}

func (pl *Player) TrackArt(track player.TrackIdentity) (image io.ReadCloser, mime string) {
	pl.withMpd(func(mpdc *mpd.Client) error {
		id := uriToMpd(track.TrackUri())
		numChunks := 0
		if strNum, err := mpdc.StickerGet(id, "image-nchunks"); err == nil {
			if num, err := strconv.ParseInt(strNum, 10, 32); err == nil {
				numChunks = int(num)
			}
		}
		if numChunks == 0 {
			return nil
		}

		chunks := make([]io.Reader, numChunks+1)
		totalLength := 0
		for i := 0; i < numChunks; i++ {
			if b64Data, err := mpdc.StickerGet(id, fmt.Sprintf("image-%v", i)); err != nil {
				return nil
			} else {
				chunks[i] = strings.NewReader(b64Data)
				totalLength += len(b64Data)
			}
		}
		// The padding seems to be getting lost somewhere along the way from MPD to here.
		chunks[len(chunks)-1] = strings.NewReader([]string{"", "=", "==", "==="}[totalLength%4])
		image = ioutil.NopCloser(base64.NewDecoder(base64.StdEncoding, io.MultiReader(chunks...)))
		mime = "image/jpeg"
		return nil
	})
	return
}

func (player *Player) Events() *util.Emitter {
	return &player.Emitter
}

func incrementPlayCount(uri string, mpdc *mpd.Client) error {
	if !strings.HasPrefix(uri, URI_SCHEMA) {
		return nil
	}

	var playCount int64
	if str, err := mpdc.StickerGet(uriToMpd(uri), "play-count"); err == nil {
		playCount, _ = strconv.ParseInt(str, 10, 32)
	}
	if err := mpdc.StickerSet(uriToMpd(uri), "play-count", strconv.FormatInt(playCount+1, 10)); err != nil {
		return fmt.Errorf("Unable to set play-count: %v", err)
	}
	return nil
}

// Helper to get an attribute as an integer from an MPD status.
func statusAttrInt(status mpd.Attrs, attr string) (int, bool) {
	if str, ok := status[attr]; ok {
		if a64, err := strconv.ParseInt(str, 10, 32); err == nil {
			return int(a64), true
		}
	}
	return 0, false
}

func uriToMpd(uri string) string {
	return strings.TrimPrefix(uri, URI_SCHEMA)
}

func mpdToUri(song string) string {
	if strings.Index(song, "://") == -1 {
		return URI_SCHEMA + song
	}
	return song
}
