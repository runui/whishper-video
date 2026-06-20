/** @type {import('./$types').PageLoad} */
import { currentTranscription } from '$lib/stores';
import { browser } from '$app/environment';
import { env } from '$env/dynamic/public';

export async function load({params}) {
    // Fetch json from /api/transcriptions/{id}
    let id = params.id;
    // Use different endpoints for server-side and client-side
    const endpoint = browser ? `${env.PUBLIC_API_HOST}/api/transcriptions` : `${env.PUBLIC_INTERNAL_API_HOST}/api/transcriptions`;
    const response = await fetch(`${endpoint}/${id}`);
    const ts = await response.json();
    if (ts.skipWhisper && ts.result?.segments?.length === 0 && ts.subtitleTracks?.length > 0) {
        ts.result = JSON.parse(JSON.stringify(ts.subtitleTracks[0].result));
        ts.result.language = ts.subtitleTracks[0].language || ts.result.language;
        ts.translations = ts.subtitleTracks[0].translations || [];
        ts.activeSubtitleTrackId = ts.subtitleTracks[0].id;
    }
    // Set currentTranscription to the fetched transcription
    currentTranscription.set(ts);
};
