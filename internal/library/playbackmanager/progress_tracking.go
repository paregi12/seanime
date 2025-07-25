package playbackmanager

import (
	"cmp"
	"context"
	"errors"
	"seanime/internal/continuity"
	discordrpc_presence "seanime/internal/discordrpc/presence"
	"seanime/internal/events"
	"seanime/internal/library/anime"
	"seanime/internal/mediaplayers/mediaplayer"
	"seanime/internal/util"

	"github.com/samber/mo"
)

var (
	ErrProgressUpdateAnilist = errors.New("playback manager: Failed to update progress on AniList")
	ErrProgressUpdateMAL     = errors.New("playback manager: Failed to update progress on MyAnimeList")
)

func (pm *PlaybackManager) listenToMediaPlayerEvents(ctx context.Context) {
	// Listen for media player events
	go func() {
		for {
			select {
			// Stop listening when the context is cancelled -- meaning a new MediaPlayer instance is set
			case <-ctx.Done():
				return
			case event := <-pm.mediaPlayerRepoSubscriber.EventCh:
				switch e := event.(type) {
				// Local file events
				case mediaplayer.TrackingStartedEvent: // New video has started playing
					pm.handleTrackingStarted(e.Status)
				case mediaplayer.VideoCompletedEvent: // Video has been watched completely but still tracking
					pm.handleVideoCompleted(e.Status)
				case mediaplayer.TrackingStoppedEvent: // Tracking has stopped completely
					pm.handleTrackingStopped(e.Reason)
				case mediaplayer.PlaybackStatusEvent: // Playback status has changed
					pm.handlePlaybackStatus(e.Status)
				case mediaplayer.TrackingRetryEvent: // Error occurred while starting tracking
					pm.handleTrackingRetry(e.Reason)

				// Streaming events
				case mediaplayer.StreamingTrackingStartedEvent:
					pm.handleStreamingTrackingStarted(e.Status)
				case mediaplayer.StreamingPlaybackStatusEvent:
					pm.handleStreamingPlaybackStatus(e.Status)
				case mediaplayer.StreamingVideoCompletedEvent:
					pm.handleStreamingVideoCompleted(e.Status)
				case mediaplayer.StreamingTrackingStoppedEvent:
					pm.handleStreamingTrackingStopped(e.Reason)
				case mediaplayer.StreamingTrackingRetryEvent:
					// Do nothing
				}
			}
		}
	}()
}

func (pm *PlaybackManager) handleTrackingStarted(status *mediaplayer.PlaybackStatus) {
	pm.eventMu.Lock()
	defer pm.eventMu.Unlock()

	// Set the playback type
	pm.currentPlaybackType = LocalFilePlayback

	// Reset the history map
	pm.historyMap = make(map[string]PlaybackState)

	// Set the current media playback status
	pm.currentMediaPlaybackStatus = status
	// Get the playback state
	_ps := pm.getLocalFilePlaybackState(status)
	// Log
	pm.Logger.Debug().Msg("playback manager: Tracking started, extracting metadata...")
	// Send event to the client
	pm.wsEventManager.SendEvent(events.PlaybackManagerProgressTrackingStarted, _ps)

	// Notify subscribers
	go func() {
		pm.playbackStatusSubscribers.Range(func(key string, value *PlaybackStatusSubscriber) bool {
			if value.canceled.Load() {
				return true
			}
			value.EventCh <- PlaybackStatusChangedEvent{Status: *status, State: _ps}
			value.EventCh <- VideoStartedEvent{Filename: status.Filename, Filepath: status.Filepath}
			return true
		})
	}()

	// Retrieve data about the current video playback
	// Set PlaybackManager.currentMediaListEntry to the list entry of the current video
	currentMediaListEntry, currentLocalFile, currentLocalFileWrapperEntry, err := pm.getLocalFilePlaybackDetails(status.Filename)
	if err != nil {
		pm.Logger.Error().Err(err).Msg("playback manager: Failed to get media data")
		// Send error event to the client
		pm.wsEventManager.SendEvent(events.ErrorToast, err.Error())
		//
		pm.MediaPlayerRepository.Cancel()
		return
	}

	pm.currentMediaListEntry = mo.Some(currentMediaListEntry)
	pm.currentLocalFile = mo.Some(currentLocalFile)
	pm.currentLocalFileWrapperEntry = mo.Some(currentLocalFileWrapperEntry)
	pm.Logger.Debug().
		Str("media", pm.currentMediaListEntry.MustGet().GetMedia().GetPreferredTitle()).
		Int("episode", pm.currentLocalFile.MustGet().GetEpisodeNumber()).
		Msg("playback manager: Playback started")

	pm.continuityManager.SetExternalPlayerEpisodeDetails(&continuity.ExternalPlayerEpisodeDetails{
		EpisodeNumber: pm.currentLocalFile.MustGet().GetEpisodeNumber(),
		MediaId:       pm.currentMediaListEntry.MustGet().GetMedia().GetID(),
		Filepath:      pm.currentLocalFile.MustGet().GetPath(),
	})

	// ------- Playlist ------- //
	go pm.playlistHub.onVideoStart(pm.currentMediaListEntry.MustGet(), pm.currentLocalFile.MustGet(), _ps)

	// ------- Discord ------- //
	if pm.discordPresence != nil && !*pm.isOffline {
		go pm.discordPresence.SetAnimeActivity(&discordrpc_presence.AnimeActivity{
			ID:            pm.currentMediaListEntry.MustGet().GetMedia().GetID(),
			Title:         pm.currentMediaListEntry.MustGet().GetMedia().GetPreferredTitle(),
			Image:         pm.currentMediaListEntry.MustGet().GetMedia().GetCoverImageSafe(),
			IsMovie:       pm.currentMediaListEntry.MustGet().GetMedia().IsMovie(),
			EpisodeNumber: pm.currentLocalFileWrapperEntry.MustGet().GetProgressNumber(pm.currentLocalFile.MustGet()),
			Progress:      int(pm.currentMediaPlaybackStatus.CurrentTimeInSeconds),
			Duration:      int(pm.currentMediaPlaybackStatus.DurationInSeconds),
		})
	}
}

func (pm *PlaybackManager) handleVideoCompleted(status *mediaplayer.PlaybackStatus) {
	pm.eventMu.Lock()
	defer pm.eventMu.Unlock()

	// Set the current media playback status
	pm.currentMediaPlaybackStatus = status
	// Get the playback state
	_ps := pm.getLocalFilePlaybackState(status)
	// Log
	pm.Logger.Debug().Msg("playback manager: Received video completed event")

	// Notify subscribers
	go func() {
		pm.playbackStatusSubscribers.Range(func(key string, value *PlaybackStatusSubscriber) bool {
			if value.canceled.Load() {
				return true
			}
			value.EventCh <- PlaybackStatusChangedEvent{Status: *status, State: _ps}
			value.EventCh <- VideoCompletedEvent{Filename: status.Filename}
			return true
		})
	}()

	//
	// Update the progress on AniList if auto update progress is enabled
	//
	pm.autoSyncCurrentProgress(&_ps)

	// Send the playback state with the `ProgressUpdated` flag
	// The client will use this to notify the user if the progress has been updated
	pm.wsEventManager.SendEvent(events.PlaybackManagerProgressVideoCompleted, _ps)
	// Push the video playback state to the history
	pm.historyMap[status.Filename] = _ps

	// ------- Playlist ------- //
	if pm.currentMediaListEntry.IsPresent() && pm.currentLocalFile.IsPresent() {
		go pm.playlistHub.onVideoCompleted(pm.currentMediaListEntry.MustGet(), pm.currentLocalFile.MustGet(), _ps)
	}
}

func (pm *PlaybackManager) handleTrackingStopped(reason string) {
	pm.eventMu.Lock()
	defer pm.eventMu.Unlock()

	pm.Logger.Debug().Msg("playback manager: Received tracking stopped event")
	pm.wsEventManager.SendEvent(events.PlaybackManagerProgressTrackingStopped, reason)

	// Find the next episode and set it to [PlaybackManager.nextEpisodeLocalFile]
	if pm.currentMediaListEntry.IsPresent() && pm.currentLocalFile.IsPresent() && pm.currentLocalFileWrapperEntry.IsPresent() {
		lf, ok := pm.currentLocalFileWrapperEntry.MustGet().FindNextEpisode(pm.currentLocalFile.MustGet())
		if ok {
			pm.nextEpisodeLocalFile = mo.Some(lf)
		} else {
			pm.nextEpisodeLocalFile = mo.None[*anime.LocalFile]()
		}
	}

	// Notify subscribers
	go func() {
		pm.playbackStatusSubscribers.Range(func(key string, value *PlaybackStatusSubscriber) bool {
			if value.canceled.Load() {
				return true
			}
			value.EventCh <- VideoStoppedEvent{Reason: reason}
			return true
		})
	}()

	if pm.currentMediaPlaybackStatus != nil {
		pm.continuityManager.UpdateExternalPlayerEpisodeWatchHistoryItem(pm.currentMediaPlaybackStatus.CurrentTimeInSeconds, pm.currentMediaPlaybackStatus.DurationInSeconds)
	}

	// ------- Playlist ------- //
	go pm.playlistHub.onTrackingStopped()

	// ------- Discord ------- //
	if pm.discordPresence != nil && !*pm.isOffline {
		go pm.discordPresence.Close()
	}
}

func (pm *PlaybackManager) handlePlaybackStatus(status *mediaplayer.PlaybackStatus) {
	pm.eventMu.Lock()
	defer pm.eventMu.Unlock()

	pm.currentPlaybackType = LocalFilePlayback

	// Set the current media playback status
	pm.currentMediaPlaybackStatus = status
	// Get the playback state
	_ps := pm.getLocalFilePlaybackState(status)
	// If the same PlaybackState is in the history, update the ProgressUpdated flag
	// PlaybackStatusCh has no way of knowing if the progress has been updated
	if h, ok := pm.historyMap[status.Filename]; ok {
		_ps.ProgressUpdated = h.ProgressUpdated
	}

	// Notify subscribers
	go func() {
		pm.playbackStatusSubscribers.Range(func(key string, value *PlaybackStatusSubscriber) bool {
			if value.canceled.Load() {
				return true
			}
			value.EventCh <- PlaybackStatusChangedEvent{Status: *status, State: _ps}
			return true
		})
	}()

	// Send the playback state to the client
	pm.wsEventManager.SendEvent(events.PlaybackManagerProgressPlaybackState, _ps)

	// ------- Playlist ------- //
	if pm.currentMediaListEntry.IsPresent() && pm.currentLocalFile.IsPresent() {
		go pm.playlistHub.onPlaybackStatus(pm.currentMediaListEntry.MustGet(), pm.currentLocalFile.MustGet(), _ps)
	}

	// ------- Discord ------- //
	if pm.discordPresence != nil && !*pm.isOffline {
		go pm.discordPresence.UpdateAnimeActivity(int(pm.currentMediaPlaybackStatus.CurrentTimeInSeconds), int(pm.currentMediaPlaybackStatus.DurationInSeconds), !pm.currentMediaPlaybackStatus.Playing)
	}
}

func (pm *PlaybackManager) handleTrackingRetry(reason string) {
	// DEVNOTE: This event is not sent to the client
	// We notify the playlist hub, so it can play the next episode (it's assumed that the user closed the player)

	// ------- Playlist ------- //
	go pm.playlistHub.onTrackingError()
}

func (pm *PlaybackManager) handleStreamingTrackingStarted(status *mediaplayer.PlaybackStatus) {
	pm.eventMu.Lock()
	defer pm.eventMu.Unlock()

	if pm.currentStreamEpisode.IsAbsent() || pm.currentStreamMedia.IsAbsent() {
		return
	}

	//// Get the media list entry
	//// Note that it might be absent if the user is watching a stream that is not in the library
	pm.currentMediaListEntry = pm.getStreamPlaybackDetails(pm.currentStreamMedia.MustGet().GetID())

	// Set the playback type
	pm.currentPlaybackType = StreamPlayback

	// Reset the history map
	pm.historyMap = make(map[string]PlaybackState)

	// Set the current media playback status
	pm.currentMediaPlaybackStatus = status
	// Get the playback state
	_ps := pm.getStreamPlaybackState(status)

	// Notify subscribers
	go func() {
		pm.playbackStatusSubscribers.Range(func(key string, value *PlaybackStatusSubscriber) bool {
			if value.canceled.Load() {
				return true
			}
			value.EventCh <- PlaybackStatusChangedEvent{Status: *status, State: _ps}
			value.EventCh <- StreamStartedEvent{Filename: status.Filename, Filepath: status.Filepath}
			return true
		})
	}()

	// Log
	pm.Logger.Debug().Msg("playback manager: Tracking started for stream")
	// Send event to the client
	pm.wsEventManager.SendEvent(events.PlaybackManagerProgressTrackingStarted, _ps)

	pm.continuityManager.SetExternalPlayerEpisodeDetails(&continuity.ExternalPlayerEpisodeDetails{
		EpisodeNumber: pm.currentStreamEpisode.MustGet().GetProgressNumber(),
		MediaId:       pm.currentStreamMedia.MustGet().GetID(),
		Filepath:      "",
	})

	// ------- Discord ------- //
	if pm.discordPresence != nil && !*pm.isOffline {
		go pm.discordPresence.SetAnimeActivity(&discordrpc_presence.AnimeActivity{
			ID:            pm.currentStreamMedia.MustGet().GetID(),
			Title:         pm.currentStreamMedia.MustGet().GetPreferredTitle(),
			Image:         pm.currentStreamMedia.MustGet().GetCoverImageSafe(),
			IsMovie:       pm.currentStreamMedia.MustGet().IsMovie(),
			EpisodeNumber: pm.currentStreamEpisode.MustGet().GetProgressNumber(),
			Progress:      int(pm.currentMediaPlaybackStatus.CurrentTimeInSeconds),
			Duration:      int(pm.currentMediaPlaybackStatus.DurationInSeconds),
		})
	}
}

func (pm *PlaybackManager) handleStreamingPlaybackStatus(status *mediaplayer.PlaybackStatus) {
	pm.eventMu.Lock()
	defer pm.eventMu.Unlock()

	if pm.currentStreamEpisode.IsAbsent() {
		return
	}

	pm.currentPlaybackType = StreamPlayback

	// Set the current media playback status
	pm.currentMediaPlaybackStatus = status
	// Get the playback state
	_ps := pm.getStreamPlaybackState(status)
	// If the same PlaybackState is in the history, update the ProgressUpdated flag
	// PlaybackStatusCh has no way of knowing if the progress has been updated
	if h, ok := pm.historyMap[status.Filename]; ok {
		_ps.ProgressUpdated = h.ProgressUpdated
	}

	// Notify subscribers
	go func() {
		pm.playbackStatusSubscribers.Range(func(key string, value *PlaybackStatusSubscriber) bool {
			if value.canceled.Load() {
				return true
			}
			value.EventCh <- PlaybackStatusChangedEvent{Status: *status, State: _ps}
			return true
		})
	}()

	// Send the playback state to the client
	pm.wsEventManager.SendEvent(events.PlaybackManagerProgressPlaybackState, _ps)

	// ------- Discord ------- //
	if pm.discordPresence != nil && !*pm.isOffline {
		go pm.discordPresence.UpdateAnimeActivity(int(pm.currentMediaPlaybackStatus.CurrentTimeInSeconds), int(pm.currentMediaPlaybackStatus.DurationInSeconds), !pm.currentMediaPlaybackStatus.Playing)
	}
}

func (pm *PlaybackManager) handleStreamingVideoCompleted(status *mediaplayer.PlaybackStatus) {
	pm.eventMu.Lock()
	defer pm.eventMu.Unlock()

	if pm.currentStreamEpisode.IsAbsent() {
		return
	}

	// Set the current media playback status
	pm.currentMediaPlaybackStatus = status
	// Get the playback state
	_ps := pm.getStreamPlaybackState(status)
	// Log
	pm.Logger.Debug().Msg("playback manager: Received video completed event")

	// Notify subscribers
	go func() {
		pm.playbackStatusSubscribers.Range(func(key string, value *PlaybackStatusSubscriber) bool {
			if value.canceled.Load() {
				return true
			}
			value.EventCh <- PlaybackStatusChangedEvent{Status: *status, State: _ps}
			value.EventCh <- StreamCompletedEvent{Filename: status.Filename}
			return true
		})
	}()
	//
	// Update the progress on AniList if auto update progress is enabled
	//
	pm.autoSyncCurrentProgress(&_ps)

	// Send the playback state with the `ProgressUpdated` flag
	// The client will use this to notify the user if the progress has been updated
	pm.wsEventManager.SendEvent(events.PlaybackManagerProgressVideoCompleted, _ps)
	// Push the video playback state to the history
	pm.historyMap[status.Filename] = _ps
}

func (pm *PlaybackManager) handleStreamingTrackingStopped(reason string) {
	pm.eventMu.Lock()
	defer pm.eventMu.Unlock()

	if pm.currentStreamEpisode.IsAbsent() {
		return
	}

	if pm.currentMediaPlaybackStatus != nil {
		pm.continuityManager.UpdateExternalPlayerEpisodeWatchHistoryItem(pm.currentMediaPlaybackStatus.CurrentTimeInSeconds, pm.currentMediaPlaybackStatus.DurationInSeconds)
	}

	// Notify subscribers
	go func() {
		pm.playbackStatusSubscribers.Range(func(key string, value *PlaybackStatusSubscriber) bool {
			if value.canceled.Load() {
				return true
			}
			value.EventCh <- StreamStoppedEvent{Reason: reason}
			return true
		})
	}()

	pm.Logger.Debug().Msg("playback manager: Received tracking stopped event")
	pm.wsEventManager.SendEvent(events.PlaybackManagerProgressTrackingStopped, reason)

	// ------- Discord ------- //
	if pm.discordPresence != nil && !*pm.isOffline {
		go pm.discordPresence.Close()
	}
}

//////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
// Local File
//////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

// getLocalFilePlaybackState returns a new PlaybackState
func (pm *PlaybackManager) getLocalFilePlaybackState(status *mediaplayer.PlaybackStatus) PlaybackState {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	currentLocalFileWrapperEntry, ok := pm.currentLocalFileWrapperEntry.Get()
	if !ok {
		return PlaybackState{}
	}

	currentLocalFile, ok := pm.currentLocalFile.Get()
	if !ok {
		return PlaybackState{}
	}

	currentMediaListEntry, ok := pm.currentMediaListEntry.Get()
	if !ok {
		return PlaybackState{}
	}

	// Find the following episode
	_, canPlayNext := currentLocalFileWrapperEntry.FindNextEpisode(currentLocalFile)

	return PlaybackState{
		EpisodeNumber:        currentLocalFileWrapperEntry.GetProgressNumber(currentLocalFile),
		AniDbEpisode:         currentLocalFile.GetAniDBEpisode(),
		MediaTitle:           currentMediaListEntry.GetMedia().GetPreferredTitle(),
		MediaTotalEpisodes:   currentMediaListEntry.GetMedia().GetCurrentEpisodeCount(),
		MediaCoverImage:      currentMediaListEntry.GetMedia().GetCoverImageSafe(),
		MediaId:              currentMediaListEntry.GetMedia().GetID(),
		Filename:             status.Filename,
		CompletionPercentage: status.CompletionPercentage,
		CanPlayNext:          canPlayNext,
	}
}

//////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
// Stream
//////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

// getStreamPlaybackState returns a new PlaybackState
func (pm *PlaybackManager) getStreamPlaybackState(status *mediaplayer.PlaybackStatus) PlaybackState {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	currentStreamEpisode, ok := pm.currentStreamEpisode.Get()
	if !ok {
		return PlaybackState{}
	}

	currentStreamMedia, ok := pm.currentStreamMedia.Get()
	if !ok {
		return PlaybackState{}
	}

	currentStreamAniDbEpisode, ok := pm.currentStreamAniDbEpisode.Get()
	if !ok {
		return PlaybackState{}
	}

	return PlaybackState{
		EpisodeNumber:        currentStreamEpisode.GetProgressNumber(),
		AniDbEpisode:         currentStreamAniDbEpisode,
		MediaTitle:           currentStreamMedia.GetPreferredTitle(),
		MediaTotalEpisodes:   currentStreamMedia.GetCurrentEpisodeCount(),
		MediaCoverImage:      currentStreamMedia.GetCoverImageSafe(),
		MediaId:              currentStreamMedia.GetID(),
		Filename:             cmp.Or(status.Filename, "Stream"),
		CompletionPercentage: status.CompletionPercentage,
		CanPlayNext:          false, // DEVNOTE: This is not used for streams
	}
}

//////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

// autoSyncCurrentProgress syncs the current video playback progress with providers.
// This is called once when a "video complete" event is heard.
func (pm *PlaybackManager) autoSyncCurrentProgress(_ps *PlaybackState) {

	shouldUpdate, err := pm.Database.AutoUpdateProgressIsEnabled()
	if err != nil {
		pm.Logger.Error().Err(err).Msg("playback manager: Failed to check if auto update progress is enabled")
		return
	}

	if !shouldUpdate {
		return
	}

	switch pm.currentPlaybackType {
	case LocalFilePlayback:
		// Note :currentMediaListEntry MUST be defined since we assume that the media is in the user's library
		if pm.currentMediaListEntry.IsAbsent() || pm.currentLocalFileWrapperEntry.IsAbsent() || pm.currentLocalFile.IsAbsent() {
			return
		}
		// Check if we should update the progress
		// If the current progress is lower than the episode progress number
		epProgressNum := pm.currentLocalFileWrapperEntry.MustGet().GetProgressNumber(pm.currentLocalFile.MustGet())
		if *pm.currentMediaListEntry.MustGet().Progress >= epProgressNum {
			return
		}

	case StreamPlayback:
		if pm.currentStreamEpisode.IsAbsent() || pm.currentStreamMedia.IsAbsent() {
			return
		}
		// Do not auto update progress is the media is in the library AND the progress is higher than the current episode
		epProgressNum := pm.currentStreamEpisode.MustGet().GetProgressNumber()
		if pm.currentMediaListEntry.IsPresent() && *pm.currentMediaListEntry.MustGet().Progress >= epProgressNum {
			return
		}
	}

	// Update the progress on AniList
	pm.Logger.Debug().Msg("playback manager: Updating progress on AniList")
	err = pm.updateProgress()

	if err != nil {
		_ps.ProgressUpdated = false
		pm.wsEventManager.SendEvent(events.ErrorToast, "Failed to update progress on AniList")
	} else {
		_ps.ProgressUpdated = true
		pm.wsEventManager.SendEvent(events.PlaybackManagerProgressUpdated, _ps)
	}

}

// SyncCurrentProgress syncs the current video playback progress with providers
// This method is called when the user manually requests to sync the progress
//   - This method will return an error only if the progress update fails on AniList
//   - This method will refresh the anilist collection
func (pm *PlaybackManager) SyncCurrentProgress() error {
	pm.eventMu.RLock()

	err := pm.updateProgress()
	if err != nil {
		pm.eventMu.RUnlock()
		return err
	}

	// Push the current playback state to the history
	if pm.currentMediaPlaybackStatus != nil {
		var _ps PlaybackState
		switch pm.currentPlaybackType {
		case LocalFilePlayback:
			pm.getLocalFilePlaybackState(pm.currentMediaPlaybackStatus)
		case StreamPlayback:
			pm.getStreamPlaybackState(pm.currentMediaPlaybackStatus)
		}
		_ps.ProgressUpdated = true
		pm.historyMap[pm.currentMediaPlaybackStatus.Filename] = _ps
		pm.wsEventManager.SendEvent(events.PlaybackManagerProgressUpdated, _ps)
	}

	pm.refreshAnimeCollectionFunc()

	pm.eventMu.RUnlock()
	return nil
}

// updateProgress updates the progress of the current video playback on AniList and MyAnimeList.
// This only returns an error if the progress update fails on AniList
//   - /!\ When this is called, the PlaybackState should have been pushed to the history
func (pm *PlaybackManager) updateProgress() (err error) {

	var mediaId int
	var epNum int
	var totalEpisodes int

	switch pm.currentPlaybackType {
	case LocalFilePlayback:
		//
		// Local File
		//
		if pm.currentLocalFileWrapperEntry.IsAbsent() || pm.currentLocalFile.IsAbsent() || pm.currentMediaListEntry.IsAbsent() {
			return errors.New("no video is being watched")
		}

		defer util.HandlePanicInModuleWithError("playbackmanager/updateProgress", &err)

		/// Online
		mediaId = pm.currentMediaListEntry.MustGet().GetMedia().GetID()
		epNum = pm.currentLocalFileWrapperEntry.MustGet().GetProgressNumber(pm.currentLocalFile.MustGet())
		totalEpisodes = pm.currentMediaListEntry.MustGet().GetMedia().GetTotalEpisodeCount() // total episode count or -1

	case StreamPlayback:
		//
		// Stream
		//
		// Last sanity check
		if pm.currentStreamEpisode.IsAbsent() || pm.currentStreamMedia.IsAbsent() {
			return errors.New("no video is being watched")
		}

		mediaId = pm.currentStreamMedia.MustGet().ID
		epNum = pm.currentStreamEpisode.MustGet().GetProgressNumber()
		totalEpisodes = pm.currentStreamMedia.MustGet().GetTotalEpisodeCount() // total episode count or -1

	case ManualTrackingPlayback:
		//
		// Manual Tracking
		//
		if pm.currentManualTrackingState.IsAbsent() {
			return errors.New("no media file is being manually tracked")
		}

		defer func() {
			if pm.manualTrackingCtxCancel != nil {
				pm.manualTrackingCtxCancel()
			}
		}()

		/// Online
		mediaId = pm.currentManualTrackingState.MustGet().MediaId
		epNum = pm.currentManualTrackingState.MustGet().EpisodeNumber
		totalEpisodes = pm.currentManualTrackingState.MustGet().TotalEpisodes

	default:
		return errors.New("unknown playback type")
	}

	if mediaId == 0 { // Sanity check
		return errors.New("media ID not found")
	}

	// Update the progress on AniList
	err = pm.platform.UpdateEntryProgress(
		context.Background(),
		mediaId,
		epNum,
		&totalEpisodes,
	)
	if err != nil {
		pm.Logger.Error().Err(err).Msg("playback manager: Error occurred while updating progress on AniList")
		return ErrProgressUpdateAnilist
	}

	pm.refreshAnimeCollectionFunc() // Refresh the AniList collection

	pm.Logger.Info().Msg("playback manager: Updated progress on AniList")

	return nil
}
