import { createApp } from 'vue'
import { createRouter, createWebHistory } from 'vue-router'
import App from './App.vue'
import CampaignList from './views/CampaignList.vue'
import CampaignNew from './views/CampaignNew.vue'
import CampaignDetail from './views/CampaignDetail.vue'
import './styles/tokens.css'
import './styles/base.css'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', component: CampaignList },
    { path: '/new', component: CampaignNew },
    { path: '/campaigns/:id', component: CampaignDetail, props: true },
  ],
})

createApp(App).use(router).mount('#app')
