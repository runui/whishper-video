import {transcriptions} from './stores';
import { dev } from '$app/environment';
import { browser } from '$app/environment';
import { env } from '$env/dynamic/public';

export let CLIENT_API_HOST = browser ? `${dev ? env.PUBLIC_API_HOST : ""}` : `${env.PUBLIC_INTERNAL_API_HOST}`;
export let CLIENT_WS_HOST = browser ? `${dev ? env.PUBLIC_API_HOST.replace("http://", "").replace("https://", "") : ""}` :  `${dev ? env.PUBLIC_INTERNAL_API_HOST.replace("http://", "").replace("https://", "") : ""}`;

// URL Validator
export const validateURL = function (url) {
    try {
        new URL(url);
        return true;
    } catch (e) {
        return false;
    }
}

export const cancelTranslation = async function (id) {
    const res = await fetch(`${CLIENT_API_HOST}/api/translate/${id}/cancel`, {
        method: "POST"
    });

    if (res.ok) {
        const updated = await res.json();
        transcriptions.update((_transcriptions) =>
            _transcriptions.map(t => t.id === updated.id ? updated : t)
        );
    }
}

export const deleteTranscription = async function (id) {
    const res = await fetch(`${CLIENT_API_HOST}/api/transcriptions/${id}`, {
        method: "DELETE"
    });

    if (res.ok) {
        transcriptions.update((_transcriptions) => _transcriptions.filter(t => t.id !== id));
    }
}

export const displayTranscriptionName = function (transcription) {
    const fileName = transcription?.fileName || '';
    if (fileName.includes('_WHSHPR_')) {
        return fileName.split('_WHSHPR_').slice(1).join('_WHSHPR_') || fileName;
    }
    return fileName || transcription?.id || 'Unknown file';
}

export const getRandomSentence = function () {
    const sentences = [
        "Audio in, text out. What's your sound about?",
        "Drop the beat, I'll drop the text!",
        "Everybody knows the bird is the word!",
        "From soundcheck to spellcheck!",
        "I got 99 problems but transcribing ain't one!",
        "I'm all ears!",
        "iTranscribe, you dictate!",
        "Lost for words?",
        "Sound check 1, 2, 3...",
        "Sound's up! What's your script?",
        "Transcribe, transcribe, transcribe!",
        "What are you transcribing today?",
        "What's the story, morning wordy?",
        "Words, don't come easy, but I can help find the way.",
        "You speak, I write. It's no magic, just AI!",
        "Can't understand that language? I can translate!",
        "I mean every word I say!",
        "What are you muttering about over there?",
        "A whisper in time saves nine subtitles.",
        "Because every word deserves a second language.",
        "Breaking the sound barrier, one word at a time.",
        "Can you hear me now? Good. Let's transcribe.",
        "Did you just say what I think you said?",
        "Don't let the silence speak for itself.",
        "Even mumbles have meaning.",
        "From babble to babblefish.",
        "Hearing is believing, transcribing is knowing.",
        "I caught that. Every. Single. Word.",
        "I heard you the first time. And the second.",
        "If a tree falls in a forest, I'll subtitle it.",
        "Let me translate that for you.",
        "Making small talk into big data.",
        "Mum's the word? Not anymore.",
        "Now with 100% more captions.",
        "Say what? Say it again, I'm transcribing.",
        "Silence is golden, but subtitles are platinum.",
        "Speak easy. I'm listening hard.",
        "Subtitle me this, Batman.",
        "The walls have ears. So do we.",
        "They say every picture tells a story. We prefer audio.",
        "This is your brain on transcription.",
        "Turns out, I'm a great listener.",
        "Wait, let me write that down.",
        "What did one subtitle say to the other? Same here.",
        "What's the frequency, Kenneth?",
        "Words are just sounds waiting to be captioned.",
        "You had me at 'Hello World'."
    ]

    const randomSentence = sentences[Math.floor(Math.random() * sentences.length)];

    return randomSentence;
}

// Content generation helpers
export function generateSRT(segments) {
    let content = '';
    segments.forEach((segment, index) => {
        let startSeconds = Math.floor(segment.start);
        let startMillis = Math.floor((segment.start - startSeconds) * 1000);
        let start = new Date(startSeconds * 1000 + startMillis).toISOString().substr(11, 12);
        let endSeconds = Math.floor(segment.end);
        let endMillis = Math.floor((segment.end - endSeconds) * 1000);
        let end = new Date(endSeconds * 1000 + endMillis).toISOString().substr(11, 12);
        content += `${index + 1}\n${start} --> ${end}\n${segment.text}\n\n`;
    });
    return content;
}

export function generateVTT(segments) {
    let content = 'WEBVTT\n\n';
    segments.forEach((segment, index) => {
        let startSeconds = Math.floor(segment.start);
        let startMillis = Math.floor((segment.start - startSeconds) * 1000);
        let start = new Date(startSeconds * 1000 + startMillis).toISOString().substr(11, 12);
        let endSeconds = Math.floor(segment.end);
        let endMillis = Math.floor((segment.end - endSeconds) * 1000);
        let end = new Date(endSeconds * 1000 + endMillis).toISOString().substr(11, 12);
        content += `${index + 1}\n${start} --> ${end}\n${segment.text}\n\n`;
    });
    return content;
}

export function generateTXT(text) {
    return text;
}

export function generateJSON(jsonData) {
    return JSON.stringify(jsonData);
}

function triggerDownload(content, filename, mimeType) {
    let blob = new Blob([content], {type: mimeType});
    let url = URL.createObjectURL(blob);
    let link = document.createElement('a');
    link.href = url;
    link.download = filename;
    link.click();
    URL.revokeObjectURL(url);
}

export const downloadSRT = function (segments, title, lang) {
    triggerDownload(generateSRT(segments), `${title}_${lang}.srt`, 'text/plain');
}

export const downloadTXT = function (text, title, lang) {
    triggerDownload(text, `${title}_${lang}.txt`, 'text/plain');
}

export const downloadJSON = function (jsonData, title, lang) {
    triggerDownload(generateJSON(jsonData), `${title}_${lang}.json`, 'text/plain');
}

export const downloadVTT = function (segments, title, lang) {
    triggerDownload(generateVTT(segments), `${title}_${lang}.vtt`, 'text/plain');
}
  
