<?php
return array(
    new FakeRoute(array('GET', 'HEAD'), 'api/faq', 'App\Http\Controllers\FaqController@index'),
    new FakeRoute(array('GET', 'HEAD'), 'api/ping', 'App\Http\Controllers\PingController'),
    new FakeRoute(array('GET', 'HEAD'), 'api/news/{slug?}', function ($slug = null) {
        return array();
    }),
    new FakeRoute(array('POST'), 'api/news', function (App\Http\Requests\StoreNewsRequest $request) {
        return array();
    }),
);
