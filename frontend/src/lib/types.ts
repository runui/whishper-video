export type Transcription = {
    id: string;
    status: number;
    language: string;
    modelSize: string;
    task: string;
    device: string;
    fileName: string;
    localPath: string;
    sourceUrl: string;
    skipWhisper: boolean;
    result: WhisperResult;
    translations: Translation[];
    subtitleTracks: SubtitleTrack[];
    translationError?: string;
    translationProgress?: TranslationProgress;
}

export type TranslationProgress = {
    total: number;
    completed: number;
    currentSegment: number;
    currentStatus: string;
}

export type WhisperResult = {
    language: string;
    duration: number;
    segments: Segment[];
    text: string;
}

export type Segment = {
    start: number;
    end: number;
    text: string;
    score: number;
    uuid?: string;
    id?: string;
    words?: WordData[];
};

type WordData = {
    start: number;
    end: number;
    word: string;
    score: number;
};

type Translation = {
    sourceLanguage: string;
    targetLanguage: string;
    translationStatus: number;
    result: WhisperResult;
    engine?: string;
};

type SubtitleTrack = {
    id: string;
    index: number;
    language: string;
    title: string;
    codec: string;
    result: WhisperResult;
    translations: Translation[];
};
