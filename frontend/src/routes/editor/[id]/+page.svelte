<script>
	/** @type {import('./$types').PageData} */
	import { onDestroy } from 'svelte';
	import { Toaster } from 'svelte-french-toast';
	import Editor from '$lib/components/Editor.svelte';
	import {currentVideoPlayerTime, currentTranscription, currentSubtitleLanguage} from '$lib/stores';
	import { CLIENT_API_HOST } from '$lib/utils';


	let video;
	let subtitleTrack;
	let emptySubtitleUrl = 'data:text/vtt,WEBVTT%0A%0A';
	let subtitleUrl;
	let tolerance = 0.1; // Tolerance level in seconds
	let canPlay = false;

	function formatVttTime(seconds) {
		const date = new Date(seconds * 1000);
		return date.toISOString().substring(11, 23);
	}

	function getSubtitleSegments(transcription, language) {
		if (!transcription) return [];
		if (language === 'original') return transcription.result?.segments || [];

		return transcription.translations
			?.find((translation) => translation.targetLanguage === language)
			?.result?.segments || [];
	}

	function hasSubtitleLanguage(transcription, language) {
		return language === 'original' || transcription.translations?.some((translation) => translation.targetLanguage === language);
	}

	function buildSubtitleUrl(segments) {
		if (subtitleUrl) URL.revokeObjectURL(subtitleUrl);

		const cues = segments
			.filter((segment) => segment.text && Number.isFinite(segment.start) && Number.isFinite(segment.end))
			.map((segment, index) => {
				const text = segment.text.replace(/-->/g, '->').trim();
				return `${index + 1}\n${formatVttTime(segment.start)} --> ${formatVttTime(segment.end)}\n${text}`;
			})
			.join('\n\n');

		subtitleUrl = URL.createObjectURL(new Blob([`WEBVTT\n\n${cues}`], { type: 'text/vtt' }));
	}

	function showSubtitles() {
		if (subtitleTrack?.track) subtitleTrack.track.mode = 'showing';
	}

	$: if ($currentTranscription && !hasSubtitleLanguage($currentTranscription, $currentSubtitleLanguage)) {
		$currentSubtitleLanguage = 'original';
	}
	$: if ($currentTranscription) buildSubtitleUrl(getSubtitleSegments($currentTranscription, $currentSubtitleLanguage));
	$: if(canPlay && video && Math.abs(video.currentTime - $currentVideoPlayerTime) > tolerance) {
		console.log(video.currentTime, $currentVideoPlayerTime)
		// When testing in Chrome, it works, just see https://stackoverflow.com/a/67584611
        video.currentTime = $currentVideoPlayerTime;
    }

	onDestroy(() => {
		if (subtitleUrl) URL.revokeObjectURL(subtitleUrl);
	});
</script>

<Toaster />
{#if $currentTranscription}
	<div class="grid h-screen grid-cols-3 overflow-hidden">
		<div class="col-span-1 overflow-hidden bg-transparent">
			<div class="relative w-full h-full">
				<video id="video" 
					   controls
					   bind:this={video}
					   on:timeupdate={(e) => $currentVideoPlayerTime = e.target.currentTime}
					   on:canplay={() => canPlay = true}
					   on:loadedmetadata={() => canPlay = true}
					   class="absolute top-0 left-0 w-full h-full">
					<source src="{CLIENT_API_HOST}/api/video/{$currentTranscription.fileName}" type="video/mp4" />
					<track
						bind:this={subtitleTrack}
						kind="captions"
						src={subtitleUrl || emptySubtitleUrl}
						srclang={$currentSubtitleLanguage === 'original' ? $currentTranscription.result.language : $currentSubtitleLanguage}
						label="Subtitles"
						default
						on:load={showSubtitles}
					/>
				</video>
			</div>
		</div>
		<div class="col-span-2 overflow-auto bg-content">
			<Editor />
		</div>
	</div>
{:else}
	<div class="flex items-center justify-center w-screen h-screen">
		<h1>
			<span class="loading loading-bars loading-lg" />
		</h1>
	</div>
{/if}
