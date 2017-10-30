package scheduler

import (
	"sync"
	"testing"
	"time"

	"code.uber.internal/infra/kraken/lib/torrent/networkevent"
	"code.uber.internal/infra/kraken/torlib"
	trackerservice "code.uber.internal/infra/kraken/tracker/service"

	"github.com/andres-erbsen/clock"
	"github.com/stretchr/testify/require"
)

func TestDownloadTorrentWithSeederAndLeecher(t *testing.T) {
	require := require.New(t)

	trackerAddr, stop := trackerservice.TestAnnouncer()
	defer stop()

	config := configFixture()

	seeder, cleanup := testPeerFixture(config, trackerAddr)
	defer cleanup()

	leecher, cleanup := testPeerFixture(config, trackerAddr)
	defer cleanup()

	tf := torlib.TestTorrentFileFixture()

	seeder.writeTorrent(tf)
	require.NoError(<-seeder.scheduler.AddTorrent(tf.MetaInfo))

	require.NoError(<-leecher.scheduler.AddTorrent(tf.MetaInfo))
	leecher.checkTorrent(t, tf)
}

func TestDownloadManyTorrentsWithSeederAndLeecher(t *testing.T) {
	require := require.New(t)

	trackerAddr, stop := trackerservice.TestAnnouncer()
	defer stop()

	config := configFixture()

	seeder, cleanup := testPeerFixture(config, trackerAddr)
	defer cleanup()

	leecher, cleanup := testPeerFixture(config, trackerAddr)
	defer cleanup()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		tf := torlib.TestTorrentFileFixture()
		wg.Add(1)
		go func() {
			defer wg.Done()

			seeder.writeTorrent(tf)
			require.NoError(<-seeder.scheduler.AddTorrent(tf.MetaInfo))

			require.NoError(<-leecher.scheduler.AddTorrent(tf.MetaInfo))
			leecher.checkTorrent(t, tf)
		}()
	}
	wg.Wait()
}

func TestDownloadManyTorrentsWithSeederAndManyLeechers(t *testing.T) {
	require := require.New(t)

	trackerAddr, stop := trackerservice.TestAnnouncer()
	defer stop()

	config := configFixture()

	seeder, cleanup := testPeerFixture(config, trackerAddr)
	defer cleanup()

	leechers, cleanup := testPeerFixtures(5, config, trackerAddr)
	defer cleanup()

	// Start seeding each torrent.
	torrentFiles := make([]*torlib.TestTorrentFile, 5)
	for i := range torrentFiles {
		tf := torlib.TestTorrentFileFixture()
		torrentFiles[i] = tf
		seeder.writeTorrent(tf)
		require.NoError(<-seeder.scheduler.AddTorrent(tf.MetaInfo))
	}

	var wg sync.WaitGroup
	for _, tf := range torrentFiles {
		tf := tf
		for _, p := range leechers {
			p := p
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case err := <-p.scheduler.AddTorrent(tf.MetaInfo):
					require.NoError(err)
					p.checkTorrent(t, tf)
				case <-time.After(10 * time.Second):
					t.Errorf("AddTorrent timeout scheduler=%s torrent=%s", p.pctx.PeerID, tf.MetaInfo.InfoHash)
				}
			}()
		}
	}
	wg.Wait()
}

func TestDownloadTorrentWhenPeersAllHaveDifferentPiece(t *testing.T) {
	require := require.New(t)

	trackerAddr, stop := trackerservice.TestAnnouncer()
	defer stop()

	config := configFixture()

	peers, cleanup := testPeerFixtures(10, config, trackerAddr)
	defer cleanup()

	pieceLength := 256
	tf := torlib.CustomTestTorrentFileFixture(len(peers)*pieceLength, pieceLength)

	var wg sync.WaitGroup
	for i, p := range peers {
		tor, err := p.torrentArchive.CreateTorrent(tf.MetaInfo)
		require.NoError(err)

		piece := make([]byte, pieceLength)
		start := i * pieceLength
		stop := (i + 1) * pieceLength
		copy(piece, tf.Content[start:stop])
		require.NoError(tor.WritePiece(piece, i))

		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case err := <-p.scheduler.AddTorrent(tf.MetaInfo):
				require.NoError(err)
				p.checkTorrent(t, tf)
			case <-time.After(10 * time.Second):
				t.Errorf("AddTorrent timeout scheduler=%s torrent=%s", p.pctx.PeerID, tf.MetaInfo.InfoHash)
			}
		}()
	}
	wg.Wait()
}

func TestPeerAnnouncesPieceAfterDownloadingFromSeeder(t *testing.T) {
	require := require.New(t)

	trackerAddr, stop := trackerservice.TestAnnouncer()
	defer stop()

	tf := torlib.TestTorrentFileFixture()

	// Each peer is allowed two connections, which allows them to establish both
	// a connection to the seeder and another peer.
	peerConfig := configFixture()
	peerConfig.ConnState.MaxOpenConnectionsPerTorrent = 2

	peerA, cleanup := testPeerFixture(peerConfig, trackerAddr)
	defer cleanup()

	peerB, cleanup := testPeerFixture(peerConfig, trackerAddr)
	defer cleanup()

	peerAErrc := peerA.scheduler.AddTorrent(tf.MetaInfo)
	peerBErrc := peerB.scheduler.AddTorrent(tf.MetaInfo)

	// Wait for peerA and peerB to establish connections to one another before
	// starting the seeder, so their handshake bitfields are both guaranteed to
	// be empty.
	waitForConnEstablished(t, peerA.scheduler, peerB.pctx.PeerID, tf.MetaInfo.InfoHash)
	waitForConnEstablished(t, peerB.scheduler, peerA.pctx.PeerID, tf.MetaInfo.InfoHash)

	// The seeder is allowed only one connection, which means only one peer will
	// have access to the completed torrent, while the other is forced to rely
	// on the "trickle down" announce piece messages.
	seederConfig := configFixture()
	seederConfig.ConnState.MaxOpenConnectionsPerTorrent = 1

	seeder, cleanup := testPeerFixture(seederConfig, trackerAddr)
	defer cleanup()

	seeder.writeTorrent(tf)
	require.NoError(<-seeder.scheduler.AddTorrent(tf.MetaInfo))
	require.NoError(<-peerAErrc)
	require.NoError(<-peerBErrc)

	peerA.checkTorrent(t, tf)
	peerB.checkTorrent(t, tf)

	// Ensure that only one peer was able to open a connection to the seeder.
	require.NotEqual(
		hasConn(peerA.scheduler, seeder.pctx.PeerID, tf.MetaInfo.InfoHash),
		hasConn(peerB.scheduler, seeder.pctx.PeerID, tf.MetaInfo.InfoHash))
}

func TestResourcesAreFreedAfterIdleTimeout(t *testing.T) {
	require := require.New(t)

	trackerAddr, stop := trackerservice.TestAnnouncer()
	defer stop()

	config := configFixture()
	config.Conn.DisableThrottling = true

	tf := torlib.TestTorrentFileFixture()
	clk := clock.NewMock()
	w := newEventWatcher()

	seeder, cleanup := testPeerFixture(config, trackerAddr, withEventLoop(w), withClock(clk))
	defer cleanup()
	seeder.writeTorrent(tf)
	require.NoError(<-seeder.scheduler.AddTorrent(tf.MetaInfo))

	leecher, cleanup := testPeerFixture(config, trackerAddr, withClock(clk))
	defer cleanup()
	errc := leecher.scheduler.AddTorrent(tf.MetaInfo)

	clk.Add(config.AnnounceInterval)

	require.NoError(<-errc)
	leecher.checkTorrent(t, tf)

	// Conns expire...
	clk.Add(config.IdleConnTTL)

	clk.Add(config.PreemptionInterval)
	w.WaitFor(t, preemptionTickEvent{})

	// Then seeding torrents expire.
	clk.Add(config.IdleSeederTTL)

	waitForTorrentRemoved(t, seeder.scheduler, tf.MetaInfo.InfoHash)
	waitForTorrentRemoved(t, leecher.scheduler, tf.MetaInfo.InfoHash)

	require.False(hasConn(seeder.scheduler, leecher.pctx.PeerID, tf.MetaInfo.InfoHash))
	require.False(hasConn(leecher.scheduler, seeder.pctx.PeerID, tf.MetaInfo.InfoHash))
}

func TestMultipleAddTorrentsForSameTorrentSucceed(t *testing.T) {
	require := require.New(t)

	trackerAddr, stop := trackerservice.TestAnnouncer()
	defer stop()

	tf := torlib.TestTorrentFileFixture()
	config := configFixture()

	seeder, cleanup := testPeerFixture(config, trackerAddr)
	defer cleanup()
	seeder.writeTorrent(tf)
	require.NoError(<-seeder.scheduler.AddTorrent(tf.MetaInfo))

	leecher, cleanup := testPeerFixture(config, trackerAddr)
	defer cleanup()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Multiple goroutines should be able to wait on the same torrent.
			require.NoError(<-leecher.scheduler.AddTorrent(tf.MetaInfo))
		}()
	}
	wg.Wait()

	leecher.checkTorrent(t, tf)

	// After the torrent is complete, further calls to AddTorrent should succeed immediately.
	require.NoError(<-leecher.scheduler.AddTorrent(tf.MetaInfo))
}

func TestEmitStatsEventTriggers(t *testing.T) {
	trackerAddr, stop := trackerservice.TestAnnouncer()
	defer stop()

	config := configFixture()
	clk := clock.NewMock()
	w := newEventWatcher()

	_, cleanup := testPeerFixture(config, trackerAddr, withEventLoop(w), withClock(clk))
	defer cleanup()

	clk.Add(config.EmitStatsInterval)
	w.WaitFor(t, emitStatsEvent{})
}

func stripTimestamps(events []networkevent.Event) []networkevent.Event {
	var res []networkevent.Event
	for _, e := range events {
		e.Time = time.Time{}
		res = append(res, e)
	}
	return res
}

func TestNetworkEvents(t *testing.T) {
	require := require.New(t)

	trackerAddr, stop := trackerservice.TestAnnouncer()
	defer stop()

	config := configFixture()
	config.IdleConnTTL = 2 * time.Second

	seeder, cleanup := testPeerFixture(config, trackerAddr)
	defer cleanup()

	leecher, cleanup := testPeerFixture(config, trackerAddr)
	defer cleanup()

	// Torrent with 1 piece.
	tf := torlib.CustomTestTorrentFileFixture(1, 1)

	seeder.writeTorrent(tf)
	require.NoError(<-seeder.scheduler.AddTorrent(tf.MetaInfo))

	require.NoError(<-leecher.scheduler.AddTorrent(tf.MetaInfo))
	leecher.checkTorrent(t, tf)

	sid := seeder.pctx.PeerID
	lid := leecher.pctx.PeerID
	h := tf.MetaInfo.InfoHash

	waitForConnRemoved(t, seeder.scheduler, lid, h)
	waitForConnRemoved(t, leecher.scheduler, sid, h)

	seederExpected := []networkevent.Event{
		networkevent.AddTorrentEvent(h, sid, []bool{true}),
		networkevent.TorrentCompleteEvent(h, sid),
		networkevent.AddConnEvent(h, sid, lid),
		networkevent.DropConnEvent(h, sid, lid),
	}

	leecherExpected := []networkevent.Event{
		networkevent.AddTorrentEvent(h, lid, []bool{false}),
		networkevent.AddConnEvent(h, lid, sid),
		networkevent.ReceivePieceEvent(h, lid, sid, 0),
		networkevent.TorrentCompleteEvent(h, lid),
		networkevent.DropConnEvent(h, lid, sid),
	}

	require.Equal(
		stripTimestamps(seederExpected),
		stripTimestamps(seeder.testProducer.Events()))

	require.Equal(
		stripTimestamps(leecherExpected),
		stripTimestamps(leecher.testProducer.Events()))
}