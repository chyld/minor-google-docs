import { mount } from 'svelte';
import '@fontsource-variable/inter';
import '@fontsource/jetbrains-mono/400.css';
import './app.css';
import App from './App.svelte';

mount(App, { target: document.getElementById('app')! });
