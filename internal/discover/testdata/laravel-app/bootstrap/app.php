<?php
$app = new FakeApp();
$app->routes = require __DIR__ . '/../routes/api.php';
return $app;
