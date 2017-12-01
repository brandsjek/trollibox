package mpd

import (
	"testing"
	"time"

	"github.com/fhs/gompd/mpd"

	"github.com/polyfloyd/trollibox/src/player"
)

func connectForTesting() (*Player, error) {
	return Connect("tcp", "127.0.0.1:6600", nil)
}

func TestPlayerImplementation(t *testing.T) {
	pl, err := connectForTesting()
	if err != nil {
		t.Skipf("%v", err)
	}
	player.TestPlayerImplementation(t, pl)
}

func TestPlaylistImplementation(t *testing.T) {
	pl, err := connectForTesting()
	if err != nil {
		t.Skipf("%v", err)
	}
	tracks, err := pl.Tracks()
	if err != nil {
		t.Fatal(err)
	}
	player.TestPlaylistImplementation(t, pl.Playlist(), tracks[:3])
}

func TestUpdateEvent(t *testing.T) {
	pl, err := connectForTesting()
	if err != nil {
		t.Skipf("%v", err)
	}

	l := pl.Events().Listen()
	defer pl.Events().Unlisten(l)
	err = pl.withMpd(func(mpdc *mpd.Client) error {
		_, err := mpdc.Update("")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	for {
		select {
		case msg := <-l:
			if msg == "tracks" {
				return
			}
		case <-time.After(time.Second * 8):
			t.Fatalf("Event \"tracks\" was not emitted")
		}
	}
}
